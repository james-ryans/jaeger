// Copyright (c) 2024 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package storagereceiver

import (
	"context"
	"fmt"

	jaeger2otlp "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/jaeger"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/cmd/jaeger/internal/extension/jaegerstorage"
	"github.com/jaegertracing/jaeger/model"
	"github.com/jaegertracing/jaeger/storage/spanstore"
)

type storageReceiver struct {
	cancelConsumeLoop context.CancelFunc
	config            *Config
	settings          receiver.CreateSettings
	consumedTraces    map[model.TraceID]*consumedTrace
	nextConsumer      consumer.Traces
	spanReader        spanstore.Reader
}

type consumedTrace struct {
	spanIDs map[model.SpanID]struct{}
}

func newTracesReceiver(config *Config, set receiver.CreateSettings, nextConsumer consumer.Traces) (*storageReceiver, error) {
	return &storageReceiver{
		config:         config,
		settings:       set,
		consumedTraces: make(map[model.TraceID]*consumedTrace),
		nextConsumer:   nextConsumer,
	}, nil
}

func (r *storageReceiver) Start(_ context.Context, host component.Host) error {
	f, err := jaegerstorage.GetStorageFactory(r.config.TraceStorage, host)
	if err != nil {
		return fmt.Errorf("cannot find storage factory: %w", err)
	}

	if r.spanReader, err = f.CreateSpanReader(); err != nil {
		return fmt.Errorf("cannot create span reader: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.cancelConsumeLoop = cancel

	go func() {
		if err := r.consumeLoop(ctx); err != nil {
			r.settings.ReportStatus(component.NewFatalErrorEvent(err))
		}
	}()

	return nil
}

func (r *storageReceiver) consumeLoop(ctx context.Context) error {
	for {
		services, err := r.spanReader.GetServices(ctx)
		if err != nil {
			r.settings.Logger.Error("Failed to get services from consumer", zap.Error(err))
			return err
		}

		for _, svc := range services {
			if err := r.consumeTraces(ctx, svc); err != nil {
				r.settings.Logger.Error("Failed to consume traces from consumer", zap.Error(err))
			}
		}

		if ctx.Err() != nil {
			r.settings.Logger.Error("Consumer stopped", zap.Error(ctx.Err()))
			return ctx.Err()
		}
	}
}

func (r *storageReceiver) consumeTraces(ctx context.Context, serviceName string) error {
	traces, err := r.spanReader.FindTraces(ctx, &spanstore.TraceQueryParameters{
		ServiceName: serviceName,
	})
	if err != nil {
		return err
	}

	for _, trace := range traces {
		traceID := trace.Spans[0].TraceID
		if _, ok := r.consumedTraces[traceID]; !ok {
			r.consumedTraces[traceID] = &consumedTrace{
				spanIDs: make(map[model.SpanID]struct{}),
			}
		}
		if len(trace.Spans) > len(r.consumedTraces[traceID].spanIDs) {
			r.consumeSpans(ctx, r.consumedTraces[traceID], trace.Spans)
		}
	}

	return nil
}

func (r *storageReceiver) consumeSpans(ctx context.Context, tc *consumedTrace, spans []*model.Span) error {
	// Spans are consumed one at a time because we don't know whether all spans
	// in a trace have been completely exported
	for _, span := range spans {
		if _, ok := tc.spanIDs[span.SpanID]; !ok {
			tc.spanIDs[span.SpanID] = struct{}{}
			td, err := jaeger2otlp.ProtoToTraces([]*model.Batch{
				{
					Spans:   []*model.Span{span},
					Process: span.Process,
				},
			})
			if err != nil {
				return err
			}
			r.nextConsumer.ConsumeTraces(ctx, td)
		}
	}

	return nil
}

func (r *storageReceiver) Shutdown(_ context.Context) error {
	r.cancelConsumeLoop()
	return nil
}
