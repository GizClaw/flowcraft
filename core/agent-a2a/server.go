package a2a

import (
	"net/http"

	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// Where the two endpoints live. The card path is the one the A2A discovery
// convention expects; the RPC path is the mount point a host may move by
// wrapping the handler in its own mux.
const (
	CardPath = "/.well-known/agent-card.json"
	RPCPath  = "/"
)

// ServerConfig is one A2A server: what it claims, how it runs, and which SDK
// options to apply.
type ServerConfig struct {
	Card     CardConfig
	Sessions Sessions
	// TaskStore, when set, makes the server read and write tasks where the agent
	// resumes from them. Left nil the SDK keeps its in-memory store, which is a
	// second record of the same task and does not survive a restart.
	TaskStore taskstore.Store
	// OnUndelivered receives input the Task did not consume, so the deployment
	// can requeue it instead of losing it.
	OnUndelivered func(conversationID string, messages []message.Message)
	// OnCancel is called after a client cancels, for work the local context
	// cannot reach — a remote peer's task, for instance.
	OnCancel func(conversationID string)
	// Options are handed to a2asrv.NewHandler: a task store, interceptors, an
	// authenticator. They are the SDK's options, not ours, so a host that needs
	// one does not have to reach around this package.
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

// NewServer builds the handler pair.
func NewServer(cfg ServerConfig) (*Server, error) {
	executor, err := New(Config{
		Card:          cfg.Card,
		Sessions:      cfg.Sessions,
		OnUndelivered: cfg.OnUndelivered,
		OnCancel:      cfg.OnCancel,
	})
	if err != nil {
		return nil, err
	}
	options := append([]a2asrv.RequestHandlerOption{}, cfg.Options...)
	if cfg.TaskStore != nil {
		options = append(options, a2asrv.WithTaskStore(cfg.TaskStore))
	}
	handler := a2asrv.NewHandler(executor, options...)
	mux := http.NewServeMux()
	mux.Handle(CardPath, a2asrv.NewStaticAgentCardHandler(executor.Card()))
	mux.Handle(RPCPath, a2asrv.NewJSONRPCHandler(handler))
	return &Server{handler: mux, card: executor.Card()}, nil
}

// ServeHTTP serves both endpoints.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Card is the card this server advertises.
func (s *Server) Card() *a2a.AgentCard { return s.card }
