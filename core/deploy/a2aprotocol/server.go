package a2aprotocol

import (
	"net/http"

	"github.com/GizClaw/flowcraft/core/agent"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/goccy/go-json"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Where the two endpoints live. The card path is the one the A2A discovery
// convention expects; the RPC path is the mount point a host may move by
// wrapping the handler in its own mux.
const (
	cardPath = "/.well-known/agent-card.json"
	rpcPath  = "/"
)

// Config is one A2A server: what it claims, how it runs, and which SDK options
// to apply.
type Config struct {
	// Name is the deployment's name. It names the card when the settings do not.
	Name string
	// Settings is the deployment's protocol block, decoded here.
	Settings json.RawMessage
	// Agent is the deployment's session table: one executor drives it directly.
	Agent *agent.Agent
	// Store is the checkpoint store; with it, tasks are read and written where
	// the agent resumes from them. Left nil the SDK keeps its in-memory store,
	// which is a second record of the same task and does not survive a restart.
	Store agent.Store
	// Fingerprint is the deployment identity a task row must carry to be restorable.
	Fingerprint agent.Fingerprint
	// ArtifactURL turns a blob reference into a URL a client can fetch. Nil keeps
	// the reference in a data part.
	ArtifactURL func(ref string) string
	// Options are handed to a2asrv.NewHandler: interceptors, an authenticator.
	// They are the SDK's options, not ours, so a host that needs one does not
	// have to reach around this package.
	Options []a2asrv.RequestHandlerOption
}

// Server mounts the JSON-RPC transport and the card endpoint.
//
// Only JSON-RPC is mounted: the SDK also speaks REST and gRPC, and a deployment
// that wants them mounts those handlers itself with the same RequestHandler.
// Mounting everything by default would publish transports nobody asked for on
// URLs nobody chose.
type Server struct {
	handler http.Handler
	card    *a2a.AgentCard
}

// New decodes the settings and builds the handler pair.
func New(cfg Config) (*Server, error) {
	settings, err := DecodeSettings(cfg.Settings)
	if err != nil {
		return nil, err
	}
	card := settings.Card
	if card.Name == "" {
		// The card is named after the agent: only the caller knows it.
		card.Name = cfg.Name
	}
	var store taskstore.Store
	if cfg.Store != nil {
		store = newTaskStore(cfg.Store, cfg.Fingerprint)
	}
	executor, err := newExecutor(executorConfig{
		Card:        card,
		Agent:       cfg.Agent,
		ArtifactURL: cfg.ArtifactURL,
	})
	if err != nil {
		return nil, err
	}
	options := append([]a2asrv.RequestHandlerOption{}, cfg.Options...)
	if store != nil {
		options = append(options, a2asrv.WithTaskStore(store))
	}
	handler := a2asrv.NewHandler(executor, options...)
	mux := http.NewServeMux()
	mux.Handle(cardPath, a2asrv.NewStaticAgentCardHandler(executor.Card()))
	mux.Handle(rpcPath, a2asrv.NewJSONRPCHandler(handler))
	return &Server{handler: propagateInbound(mux), card: executor.Card()}, nil
}

// propagateInbound extracts the caller's trace context from the request headers
// into the context the executor runs under, so the run joins the caller's trace
// instead of starting one of its own.
//
// It starts no span. The root of a turn belongs to the host: a server span
// invented here would duplicate whatever the host's own HTTP middleware opened,
// and a wrongly-parented root is worse than a missing one. The SDK threads
// req.Context() into the executor, so extracting here is enough for agent.run —
// and everything below it — to hang off the caller's span.
//
// With no propagator installed this is a pass-through: Extract on the no-op
// propagator returns the context it was given.
func propagateInbound(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Handler serves the card and the JSON-RPC endpoint.
func (s *Server) Handler() http.Handler { return s.handler }

// ServeHTTP makes the server usable as a handler without a second wrapper.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Card is the AgentCard this server answers with.
func (s *Server) Card() *a2a.AgentCard { return s.card }

// Close releases nothing: the server holds no background work. It exists so a
// protocol kind can be closed uniformly with an engine.
func (s *Server) Close() error { return nil }
