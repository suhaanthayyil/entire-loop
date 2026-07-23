// Package state persists the run state of a loop to disk as JSON. A run is a
// bounded sequence of rounds; each round records the seat outcomes, the metrics
// the measure seat computed, and the verdict. The store lives under the plugin
// data dir at runs/<runID>/state.json and is rewritten after every round so a
// crashed run leaves a readable, resumable trail.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SeatOutcome is the loop-facing projection of one worker seat's result. It is
// deliberately decoupled from the agent package so state has no dependency on
// the exec/argv machinery.
type SeatOutcome struct {
	Role     string   `json:"role"`
	OK       bool     `json:"ok"`
	Warnings []string `json:"warnings,omitempty"`
	CostUSD  float64  `json:"cost_usd"`
	NumTurns int      `json:"num_turns,omitempty"`
	Findings []string `json:"findings,omitempty"`
	Proposal string   `json:"proposal,omitempty"`
	// ProposalKey is the stable dedupe key for this seat's proposal, persisted so a
	// process-resume can rebuild the convergence `seen` set even when the proposal
	// text itself was dropped (e.g. a verifier-refuted build proposal is cleared
	// from Proposal but its key is retained here, so it never resurfaces on resume).
	ProposalKey string             `json:"proposal_key,omitempty"`
	Metrics     map[string]float64 `json:"metrics,omitempty"`
	Verdict     string             `json:"verdict,omitempty"`
	GoalMet     bool               `json:"goal_met,omitempty"`
}

// RoundState captures everything produced in a single round of the loop.
type RoundState struct {
	Round     int                `json:"round"`
	Seats     []SeatOutcome      `json:"seats"`
	Metrics   map[string]float64 `json:"metrics,omitempty"`
	Verdict   string             `json:"verdict,omitempty"`
	Route     string             `json:"route,omitempty"`
	GoalMet   bool               `json:"goal_met"`
	CostUSD   float64            `json:"cost_usd"`
	StartedAt time.Time          `json:"started_at"`
	EndedAt   time.Time          `json:"ended_at"`
}

// FrozenSeat is the persisted description of one seat in an immutable-plan-mode
// roster. It mirrors the seat-defining fields of agent.SeatSpec but lives in the
// state package so state keeps NO dependency on the agent exec/argv machinery (the
// org package converts between the two). Round is intentionally omitted — the loop
// re-stamps the round onto the roster every round.
type FrozenSeat struct {
	Role      string `json:"role"`
	BriefOnly bool   `json:"brief_only,omitempty"`
	McpBrain  bool   `json:"mcp_brain,omitempty"`
	Mutating  bool   `json:"mutating,omitempty"`
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
	RepoRoot  string `json:"repo_root,omitempty"`
	Lens      string `json:"lens,omitempty"`
	Focus     string `json:"focus,omitempty"`
}

// State is the full persisted record of a loop run.
//
// RefinedGoal and Subgoals are the LLM control plane's evolving restatement of the
// goal: the control seat refines them each round and they are persisted here so
// (a) subsequent worker seats' briefs carry the sharpened goal and (b) the next
// round's control seat builds on its own prior planning rather than restarting.
// They are empty under the fixed planner.
//
// FrozenSeats is the immutable-plan-mode roster, decided once on the first round
// and persisted so a RESUME (a new process) rehydrates the SAME DAG instead of
// re-planning and re-freezing a different one. It is empty under dynamic plan-mode.
type State struct {
	SchemaVersion int                `json:"schema_version"`
	RunID         string             `json:"run_id"`
	Goal          string             `json:"goal"`
	RefinedGoal   string             `json:"refined_goal,omitempty"`
	Subgoals      []string           `json:"subgoals,omitempty"`
	Round         int                `json:"round"`
	Rounds        []RoundState       `json:"rounds"`
	Metrics       map[string]float64 `json:"metrics,omitempty"`
	FrozenSeats   []FrozenSeat       `json:"frozen_seats,omitempty"`
	GoalMet       bool               `json:"goal_met"`
	TotalCostUSD  float64            `json:"total_cost_usd"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

// schemaVersion is bumped when the on-disk shape changes incompatibly.
const schemaVersion = 1

// Store owns the on-disk location for a single run's state.
type Store struct {
	dir   string // <dataDir>/runs/<runID>
	runID string
}

// NewStore builds a store for runID rooted at dataDir. It does not touch disk
// until Save is called (or MkdirAll via ensureDir).
func NewStore(dataDir, runID string) *Store {
	return &Store{
		dir:   filepath.Join(dataDir, "runs", runID),
		runID: runID,
	}
}

// Dir returns the run directory (created lazily on first Save).
func (s *Store) Dir() string { return s.dir }

// path is the state.json location.
func (s *Store) path() string { return filepath.Join(s.dir, "state.json") }

// NewState seeds a fresh State for goal at time now.
func (s *Store) NewState(goal string, now time.Time) *State {
	return &State{
		SchemaVersion: schemaVersion,
		RunID:         s.runID,
		Goal:          goal,
		Round:         0,
		Metrics:       map[string]float64{},
		CreatedAt:     now.UTC(),
		UpdatedAt:     now.UTC(),
	}
}

// Load reads the persisted state. A missing file is not an error — it returns
// (nil, nil) so callers can fall back to NewState.
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", s.path(), err)
	}
	return &st, nil
}

// Save writes the state atomically (temp file + rename) so a partial write can
// never corrupt an existing state.json.
func (s *Store) Save(st *State) error {
	if st == nil {
		return errors.New("save: nil state")
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	st.SchemaVersion = schemaVersion
	st.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, "state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpName, s.path()); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

func (s *Store) ensureDir() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create run dir %s: %w", s.dir, err)
	}
	return nil
}

// Merge folds a completed round into the state: it appends the round, advances
// the round counter, rolls the round's metrics into the run-level metrics (last
// value wins per key), accumulates cost, and promotes the round's goal-met flag.
// Merge is pure with respect to disk — the caller Saves afterward.
func Merge(st *State, round RoundState) {
	if st == nil {
		return
	}
	if st.Metrics == nil {
		st.Metrics = map[string]float64{}
	}
	st.Rounds = append(st.Rounds, round)
	st.Round = round.Round
	for k, v := range round.Metrics {
		st.Metrics[k] = v
	}
	st.TotalCostUSD += round.CostUSD
	if round.GoalMet {
		st.GoalMet = true
	}
	st.UpdatedAt = time.Now().UTC()
}
