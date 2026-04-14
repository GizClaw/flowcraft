package telemetry

import (
	"context"
	"errors"
	"fmt"
)

type initAllOpts struct {
	trace []TraceOption
	meter []MeterOption
	log   []LogOption
}

type Option func(*initAllOpts)

func TracerOpts(opts ...TraceOption) Option {
	return func(c *initAllOpts) {
		c.trace = append(c.trace, opts...)
	}
}

func MeterOpts(opts ...MeterOption) Option {
	return func(c *initAllOpts) {
		c.meter = append(c.meter, opts...)
	}
}

func LoggerOpts(opts ...LogOption) Option {
	return func(c *initAllOpts) {
		c.log = append(c.log, opts...)
	}
}

// InitAll initializes tracing, metrics, and logging in one call.
// It returns a single shutdown function that tears down all three
// in reverse order.
func InitAll(ctx context.Context, opts ...Option) (shutdown func(context.Context) error, err error) {
	cfg := &initAllOpts{}
	for _, fn := range opts {
		if fn != nil {
			fn(cfg)
		}
	}

	traceShutdown, err := InitTracer(ctx, cfg.trace...)
	if err != nil {
		return nil, fmt.Errorf("init tracer: %w", err)
	}
	meterShutdown, err := InitMeter(ctx, cfg.meter...)
	if err != nil {
		traceShutdown(ctx)
		return nil, fmt.Errorf("init meter: %w", err)
	}
	logShutdown, err := InitLog(ctx, cfg.log...)
	if err != nil {
		meterShutdown(ctx)
		traceShutdown(ctx)
		return nil, fmt.Errorf("init log: %w", err)
	}
	shutdown = func(ctx context.Context) error {
		var errs []error
		if e := logShutdown(ctx); e != nil {
			errs = append(errs, e)
		}
		if e := meterShutdown(ctx); e != nil {
			errs = append(errs, e)
		}
		if e := traceShutdown(ctx); e != nil {
			errs = append(errs, e)
		}
		return errors.Join(errs...)
	}
	return shutdown, nil
}
