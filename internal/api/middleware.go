package api

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/policy"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
)

var (
	apiMeter = telemetry.MeterWithSuffix("api")

	apiRequestCount, _    = apiMeter.Int64Counter("requests.total", metric.WithDescription("Total API requests"))
	apiRequestDuration, _ = apiMeter.Float64Histogram("duration.seconds", metric.WithDescription("API request duration"))
	apiErrorCount, _      = apiMeter.Int64Counter("errors.total", metric.WithDescription("Total API errors"))
)

func (s *Server) middleware(next http.Handler) http.Handler {
	h := next
	h = s.corsMiddleware(h)
	h = s.securityHeadersMiddleware(h)
	h = s.maxBodySizeMiddleware(h)
	h = s.jwtMiddleware(h)
	h = s.rateLimitMiddleware(h)
	h = s.recoveryMiddleware(h)
	h = s.loggingMiddleware(h)
	h = s.otelMiddleware(h)
	return h
}

// otelMiddleware creates a root span, records metrics, and logs each request.
func (s *Server) otelMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := telemetry.Tracer().Start(r.Context(), "api."+r.Method+"."+r.URL.Path)

		defer span.End()

		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))
		dur := time.Since(start)

		routeAttrs := metric.WithAttributes(
			attribute.String("method", r.Method),
			attribute.String("path", r.URL.Path),
			attribute.Int("status", rw.status),
		)
		apiRequestCount.Add(ctx, 1, routeAttrs)
		apiRequestDuration.Record(ctx, dur.Seconds(), routeAttrs)
		if rw.status >= 400 {
			apiErrorCount.Add(ctx, 1, routeAttrs)
		}

		span.SetAttributes(
			attribute.Int("http.status_code", rw.status),
			attribute.Float64("http.duration_s", dur.Seconds()),
		)

		telemetry.Info(ctx, "api request",
			otellog.String("method", r.Method),
			otellog.String("path", r.URL.Path),
			otellog.Int("status", rw.status),
			otellog.String("duration", fmt.Sprintf("%dms", dur.Milliseconds())))
	})
}

// loggingMiddleware is now merged into otelMiddleware for unified observability.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return next
}

// rateLimitMiddleware implements per-IP token bucket rate limiting.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	if s.config.RateLimitRPS <= 0 {
		return next
	}

	type bucket struct {
		tokens    float64
		lastCheck time.Time
	}

	var mu sync.Mutex
	buckets := make(map[string]*bucket)
	rps := s.config.RateLimitRPS
	burst := s.config.RateLimitBurst
	if burst <= 0 {
		burst = int(rps) * 2
		if burst < 10 {
			burst = 10
		}
	}

	cleanupInterval := s.config.RateLimitCleanupInterval
	if cleanupInterval <= 0 {
		cleanupInterval = 5
	}
	bucketExpiry := s.config.RateLimitBucketExpiry
	if bucketExpiry <= 0 {
		bucketExpiry = 10
	}
	expiryDuration := time.Duration(bucketExpiry) * time.Minute

	go func() {
		ticker := time.NewTicker(time.Duration(cleanupInterval) * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				mu.Lock()
				now := time.Now()
				for ip, b := range buckets {
					if now.Sub(b.lastCheck) > expiryDuration {
						delete(buckets, ip)
					}
				}
				mu.Unlock()
			}
		}
	}()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if idx := strings.LastIndex(ip, ":"); idx > 0 {
			ip = ip[:idx]
		}

		mu.Lock()
		b, ok := buckets[ip]
		if !ok {
			b = &bucket{tokens: float64(burst), lastCheck: time.Now()}
			buckets[ip] = b
		}
		now := time.Now()
		elapsed := now.Sub(b.lastCheck).Seconds()
		b.lastCheck = now
		b.tokens += elapsed * rps
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}

		if b.tokens < 1 {
			mu.Unlock()
			writeError(w, errdefs.RateLimitf("rate limit exceeded"))
			return
		}
		b.tokens--
		mu.Unlock()

		next.ServeHTTP(w, r)
	})
}

// jwtMiddleware validates JWT tokens for protected API routes.
func (s *Server) jwtMiddleware(next http.Handler) http.Handler {
	if s.jwt == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/webhook/") {
			next.ServeHTTP(w, r)
			return
		}
		// /api/events/ws does its own ticket-based auth (§12.3) and must
		// bypass the cookie/JWT check; the ws-ticket already encodes the
		// authorized actor + (partition, since) bound to it.
		if r.URL.Path == "/api/events/ws" {
			next.ServeHTTP(w, r)
			return
		}

		claims, ok := s.authenticateRequest(r)
		if !ok {
			writeError(w, errdefs.Unauthorizedf("unauthorized"))
			return
		}
		// FlowCraft is single-owner: every authenticated subject is the
		// super admin. Inject the actor so policy.ActorFrom downstream
		// (admin endpoints, event-stream gates) sees a SuperAdmin.
		if claims != nil {
			actor := policy.Actor{
				Type:  policy.ActorUser,
				ID:    claims.Username,
				Super: true,
			}
			r = r.WithContext(policy.WithActor(r.Context(), actor))
		}
		next.ServeHTTP(w, r)
	})
}

func trimBearerToken(auth string) string {
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

// corsMiddleware adds CORS headers.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	allowAll := len(s.config.CORSOrigins) == 0

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		if !allowAll && origin != "" {
			valid := false
			for _, o := range s.config.CORSOrigins {
				if o == origin {
					valid = true
					break
				}
			}
			if valid {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
		} else if allowAll {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Expose-Headers", "X-Warning")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeadersMiddleware sets standard security response headers.
func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// recoveryMiddleware catches panics and returns 500 instead of crashing.
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				telemetry.Error(r.Context(), "api: panic recovered",
					otellog.String("panic", fmt.Sprintf("%v", rv)),
					otellog.String("method", r.Method),
					otellog.String("path", r.URL.Path))
				writeError(w, errdefs.Internalf("internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// maxBodySizeMiddleware limits the request body size.
func (s *Server) maxBodySizeMiddleware(next http.Handler) http.Handler {
	maxSize := s.config.MaxBodySize
	if maxSize <= 0 {
		maxSize = 10 * 1024 * 1024
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxSize)
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support Hijack")
}

func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}
