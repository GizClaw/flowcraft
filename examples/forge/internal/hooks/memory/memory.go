// Package memory gives a seat its own recollection across turns.
//
// Delegation starts a fresh child session for every call, so a seat cannot rely on
// its transcript: it is asked a question, it answers, and the session ends. What it
// said before, and what it was told, has to live somewhere the deployment owns -
// the workspace, through the agent's own fsx tree.
//
// The file is per game and per seat. A game is identified by the seed the referee
// put in the brief (`game=<seed>`), which is what makes a new game start from a
// clean page and a replayed seed reproduce the same one.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// KindName is the kind a deployment names in its hooks list.
const KindName = "seat-memory"

// Settings is the kind's written configuration.
type Settings struct {
	// Dir is the workspace-relative directory the files live under.
	Dir string `json:"dir"`
	// Keep is how many past lines are replayed into the next turn.
	Keep int `json:"keep"`
}

// Kind returns the hook kind: one seat's memory, read before a turn and written
// after it.
func Kind() deploy.HookKind {
	return deploy.HookKind{
		Name: KindName,
		Decode: func(settings json.RawMessage) (any, error) {
			return decode(settings)
		},
		Build: build,
	}
}

// Defaults the settings fill in.
const (
	defaultDir  = ".forge/memory"
	defaultKeep = 12
)

func decode(raw json.RawMessage) (Settings, error) {
	var settings Settings
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return Settings{}, err
		}
	}
	if settings.Dir == "" {
		settings.Dir = defaultDir
	}
	if settings.Keep <= 0 {
		settings.Keep = defaultKeep
	}
	if path.IsAbs(settings.Dir) || strings.HasPrefix(settings.Dir, "..") {
		return Settings{}, fmt.Errorf("dir %q must be a workspace-relative path", settings.Dir)
	}
	return settings, nil
}

// entry is one remembered exchange: what the seat was asked and what it said.
type entry struct {
	Asked string `json:"asked"`
	Said  string `json:"said"`
}

func build(ctx context.Context, deps deploy.HookDeps) (agent.Hooks, error) {
	settings, err := decode(deps.Spec.Settings)
	if err != nil {
		return agent.Hooks{}, errdefs.Validationf("seat-memory: %v", err)
	}
	if deps.Runtime == nil || len(deps.Runtime.Files().Writable()) == 0 {
		return agent.Hooks{}, errdefs.Validationf(
			"seat-memory: agent %q has no writable runtime, so there is nowhere to keep a memory",
			deps.Agent)
	}
	seat := seatOf(deps.Agent)
	return agent.Hooks{
		BeforeTurn: func(ctx context.Context, turn *agent.Turn) error {
			game := rootSession(turn.SessionID)
			if gameOf(turn.Message) == "" || game == "" {
				// A brief without a game id is not this hook's business: better to
				// answer without memory than to read another game's page.
				return nil
			}
			if kindOf(turn.Message) == kindPostgame {
				// A recap is asked after the table is over. What a seat remembers is a
				// list of night actions, and handing it to the model as context turns
				// "who were the wolves?" into "who do we knife?".
				return nil
			}
			kept, err := load(ctx, deps, settings, game, seat)
			if err != nil {
				return fmt.Errorf("seat-memory: %s: %w", turn.Agent, err)
			}
			if len(kept) == 0 {
				return nil
			}
			turn.Message = withRecap(turn.Message, kept)
			return nil
		},
		AfterTurn: func(ctx context.Context, turn agent.Turn, result agent.Result) error {
			game := rootSession(turn.SessionID)
			if gameOf(turn.Message) == "" || game == "" || kindOf(turn.Message) == kindPostgame {
				return nil
			}
			said := strings.TrimSpace(result.Content.Text())
			if said == "" {
				return nil
			}
			kept, err := load(ctx, deps, settings, game, seat)
			if err != nil {
				return err
			}
			kept = append(kept, entry{Asked: briefOf(turn.Message), Said: said})
			if len(kept) > settings.Keep {
				kept = kept[len(kept)-settings.Keep:]
			}
			return save(ctx, deps, settings, game, seat, kept)
		},
	}, nil
}

// seatOf turns a deployment name into the seat it plays: "seat_5" is "5".
func seatOf(agentName string) string {
	if _, id, qualified := strings.Cut(agentName, "_"); qualified && id != "" {
		return id
	}
	return agentName
}

// gameOf reads the game id the referee writes at the top of a brief.
func gameOf(msg message.Message) string {
	for _, line := range strings.Split(msg.Content.Text(), "\n") {
		line = strings.TrimSpace(line)
		if rest, found := strings.CutPrefix(line, "game="); found {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// rootSession is the session a seat's delegations belong to: the one that owns the
// table, not the child session a single delegation mints.
//
// A seat's own session id is fresh per call - it is the parent's session, the
// delegation marker and the call that asked - so keying memory by it would remember
// nothing. Keying it by the game's seed is no better: the seed is the opening line,
// and two games that open the same way would share a page. The root session is both:
// stable across the delegations of one game and different for the next one.
func rootSession(sessionID string) string {
	root, _, _ := strings.Cut(sessionID, "/d/")
	root = strings.TrimSpace(root)
	if root == "" || root == sessionID {
		// Not a delegated session: there is no table behind it to belong to.
		return ""
	}
	return strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(root)
}

// kindPostgame is the brief kind that asks for a recap instead of a move.
const kindPostgame = "postgame"

// kindOf reads the second contract line of a brief, or "" when it has none.
func kindOf(msg message.Message) string {
	for _, line := range strings.Split(msg.Content.Text(), "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), "kind="); found {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// briefOf is the question without the memory this hook injected: storing the recap
// back into the file would grow it by one copy per turn.
func briefOf(msg message.Message) string {
	text := msg.Content.Text()
	if index := strings.Index(text, recapHeader); index >= 0 {
		text = text[:index]
	}
	return strings.TrimSpace(text)
}

// withRecap prepends what the seat remembers to the turn it is about to answer.
func withRecap(msg message.Message, kept []entry) message.Message {
	// The memory goes first and the brief stays last: the model acts on the end of its
	// context, and a brief followed by a list of old actions reads as "continue the
	// night" - which is how a recap question used to get an answer about who to knife.
	var out strings.Builder
	out.WriteString(recapHeader)
	for _, item := range kept {
		out.WriteString("\n- 你之前说：" + item.Said)
	}
	out.WriteString("\n\n")
	out.WriteString(msg.Content.Text())
	updated := message.NewTextMessage(msg.Role, out.String())
	updated.Metadata = msg.Metadata
	return updated
}

// recapHeader marks the injected section, so it can be told apart from the brief.
const recapHeader = "你的记忆（你之前在这个座位上说过的话）："

func load(ctx context.Context, deps deploy.HookDeps, settings Settings, game, seat string) ([]entry, error) {
	body, err := deps.Runtime.Files().Read(ctx, file(settings, game, seat, deps))
	if err != nil {
		// Nothing remembered yet is the ordinary first turn, not a failure. fsx
		// reports the missing file as the OS does, so the check is the OS one.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read: %w", err)
	}
	var kept []entry
	if err := json.Unmarshal(body, &kept); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return kept, nil
}

func save(ctx context.Context, deps deploy.HookDeps, settings Settings, game, seat string, kept []entry) error {
	body, err := json.Marshal(kept)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	dir := path.Join(deps.Runtime.Files().Writable()[0], settings.Dir, game)
	// MkdirAll semantics: an existing directory is the ordinary case.
	if err := deps.Runtime.Files().Mkdir(ctx, dir); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return deps.Runtime.Files().Write(ctx, file(settings, game, seat, deps), body)
}

// file is the absolute path of one seat's page in one game.
func file(settings Settings, game, seat string, deps deploy.HookDeps) string {
	return path.Join(deps.Runtime.Files().Writable()[0], settings.Dir, game, "seat_"+seat+".json")
}
