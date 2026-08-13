// Package team routes transport-neutral messages through persona agent pools.
package team

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/your-company/claude-team-harness/internal/acp"
	"github.com/your-company/claude-team-harness/internal/conversation"
	"github.com/your-company/claude-team-harness/internal/mcpprofile"
	"github.com/your-company/claude-team-harness/internal/persona"
	"github.com/your-company/claude-team-harness/internal/state"
)

type StartFunc func(
	context.Context, persona.Persona, []acp.MCPServer, func(acp.Event),
) (conversation.SessionClient, io.Closer, error)

type Config struct {
	Roster    *persona.Set
	Profiles  *mcpprofile.Set
	Store     *state.Store
	Cwd       string
	MaxTurns  int
	MaxAgents int
	Start     StartFunc
	Logf      func(string, ...any)
}

type Runtime struct {
	cfg   Config
	pools map[string]*pool
}

type PersonaInfo struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
}

type pool struct {
	persona persona.Persona
	store   *state.Store
	logf    func(string, ...any)
	global  chan struct{}
	slots   []*slot

	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	assigned map[string]*slot
	nextSlot int
}

type slot struct {
	manager *conversation.Manager
	closer  io.Closer
	permit  chan struct{}

	mu        sync.Mutex
	activeRun string
}

func New(ctx context.Context, cfg Config) (*Runtime, error) {
	if cfg.Roster == nil || cfg.Profiles == nil || cfg.Store == nil || cfg.Start == nil || cfg.Cwd == "" {
		return nil, errors.New("team runtime needs roster, profiles, store, start function, and working directory")
	}
	if cfg.MaxTurns <= 0 {
		return nil, errors.New("team runtime max turns must be positive")
	}
	if cfg.MaxAgents <= 0 {
		cfg.MaxAgents = 8
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	enabled := cfg.Roster.Enabled()
	totalSlots := 0
	profileNames := make([]string, 0, len(enabled))
	for _, member := range enabled {
		totalSlots += member.Slots
		profileNames = append(profileNames, member.MCPProfile)
	}
	if totalSlots > cfg.MaxAgents {
		return nil, fmt.Errorf("persona roster requests %d slots; max agents is %d", totalSlots, cfg.MaxAgents)
	}
	if err := cfg.Profiles.Validate(profileNames); err != nil {
		return nil, err
	}
	runtime := &Runtime{cfg: cfg, pools: make(map[string]*pool, len(enabled))}
	global := make(chan struct{}, cfg.MaxAgents)
	for _, member := range enabled {
		created, err := newPool(ctx, cfg, member, global)
		if err != nil {
			_ = runtime.Close()
			return nil, err
		}
		runtime.pools[member.Name] = created
	}
	return runtime, nil
}

func newPool(ctx context.Context, cfg Config, member persona.Persona, global chan struct{}) (*pool, error) {
	servers, err := cfg.Profiles.Resolve(member.MCPProfile)
	if err != nil {
		return nil, fmt.Errorf("persona %s: %w", member.Name, err)
	}
	created := &pool{
		persona: member, store: cfg.Store, logf: cfg.Logf, global: global,
		locks: make(map[string]*sync.Mutex), assigned: make(map[string]*slot),
	}
	for index := 0; index < member.Slots; index++ {
		worker := &slot{permit: make(chan struct{}, 1)}
		eventContext := context.WithoutCancel(ctx)
		onEvent := func(event acp.Event) {
			worker.mu.Lock()
			runID := worker.activeRun
			worker.mu.Unlock()
			if runID == "" {
				return
			}
			if err := cfg.Store.AddRuntimeEvent(
				eventContext, runID, member.Name, event.Kind,
				event.SessionID, "observed",
			); err != nil {
				cfg.Logf("record runtime event for %s: %v", member.Name, err)
			}
		}
		client, closer, err := cfg.Start(ctx, member, servers, onEvent)
		if err != nil {
			created.close()
			return nil, fmt.Errorf("start persona %s slot %d: %w", member.Name, index, err)
		}
		worker.closer = closer
		manager, err := conversation.New(conversation.Config{
			Client: client, Store: cfg.Store, Cwd: cfg.Cwd, Servers: servers,
			MaxTurns: cfg.MaxTurns, Persona: member.Name, PersonaPrompt: member.Prompt,
			OnActive: func(runID, _ string, active bool) {
				worker.mu.Lock()
				if active {
					worker.activeRun = runID
				} else {
					worker.activeRun = ""
				}
				worker.mu.Unlock()
			},
			Logf: cfg.Logf,
		})
		if err != nil {
			_ = closer.Close()
			created.close()
			return nil, err
		}
		worker.manager = manager
		created.slots = append(created.slots, worker)
	}
	return created, nil
}

func (r *Runtime) Handle(ctx context.Context, input conversation.Input) (conversation.Result, error) {
	baseKey := input.Scope.Key
	member, err := r.route(ctx, input.Persona, baseKey, input.Text)
	if err != nil {
		return conversation.Result{}, err
	}
	input.Persona = member.Name
	input.Scope.Key = qualify(member.Name, baseKey)
	selected := r.pools[member.Name]
	if input.Mode != conversation.ModeFresh {
		if result, delivered, steerErr := selected.steer(ctx, input); delivered {
			return result, nil
		} else if steerErr != nil {
			r.cfg.Logf("steer %s: %v; queueing a normal turn", input.Scope.Key, steerErr)
		}
	}
	return selected.handle(ctx, input)
}

func (r *Runtime) route(ctx context.Context, explicit, baseKey, text string) (persona.Persona, error) {
	if explicit != "" {
		member, found := r.cfg.Roster.Lookup(explicit)
		if !found {
			return persona.Persona{}, persona.UnknownError{Name: explicit}
		}
		if err := r.cfg.Store.PutRoute(ctx, baseKey, member.Name); err != nil {
			return persona.Persona{}, err
		}
		return member, nil
	}
	saved, err := r.cfg.Store.Route(ctx, baseKey)
	if err != nil {
		return persona.Persona{}, err
	}
	member := r.cfg.Roster.Route(text, saved)
	if err := r.cfg.Store.PutRoute(ctx, baseKey, member.Name); err != nil {
		return persona.Persona{}, err
	}
	return member, nil
}

func (p *pool) handle(ctx context.Context, input conversation.Input) (conversation.Result, error) {
	lock := p.scopeLock(input.Scope.Key)
	lock.Lock()
	defer lock.Unlock()
	select {
	case p.global <- struct{}{}:
		defer func() { <-p.global }()
	case <-ctx.Done():
		return conversation.Result{}, ctx.Err()
	}
	worker := p.slotFor(input.Scope.Key)
	select {
	case worker.permit <- struct{}{}:
		defer func() { <-worker.permit }()
	case <-ctx.Done():
		return conversation.Result{}, ctx.Err()
	}
	return worker.manager.Handle(ctx, input)
}

func (p *pool) steer(
	ctx context.Context, input conversation.Input,
) (conversation.Result, bool, error) {
	for _, worker := range p.slots {
		result, delivered, err := worker.manager.Steer(ctx, input)
		if delivered || err != nil {
			if delivered {
				if eventErr := p.store.AddRuntimeEvent(
					context.WithoutCancel(ctx), result.RunID, p.persona.Name,
					"actor.steer", result.SessionID, "delivered",
				); eventErr != nil {
					p.logf("record steer event for %s: %v", p.persona.Name, eventErr)
				}
			}
			return result, delivered, err
		}
	}
	return conversation.Result{}, false, nil
}

func (p *pool) scopeLock(key string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.locks[key] == nil {
		p.locks[key] = &sync.Mutex{}
	}
	return p.locks[key]
}

func (p *pool) slotFor(key string) *slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	if worker := p.assigned[key]; worker != nil {
		return worker
	}
	worker := p.slots[p.nextSlot%len(p.slots)]
	p.nextSlot++
	p.assigned[key] = worker
	return worker
}

func (r *Runtime) Personas() []PersonaInfo {
	members := r.cfg.Roster.Enabled()
	result := make([]PersonaInfo, 0, len(members))
	for _, member := range members {
		result = append(result, PersonaInfo{
			Name: member.Name, DisplayName: member.DisplayName,
			Description: member.Description, Default: member.Default,
		})
	}
	return result
}

func (r *Runtime) ValidatePersona(name string) error {
	if name == "" {
		return nil
	}
	if _, found := r.cfg.Roster.Lookup(name); !found {
		return persona.UnknownError{Name: name}
	}
	return nil
}

func (r *Runtime) Close() error {
	var result error
	for _, created := range r.pools {
		result = errors.Join(result, created.close())
	}
	return result
}

func (p *pool) close() error {
	var result error
	for _, worker := range p.slots {
		result = errors.Join(result, worker.closer.Close())
	}
	return result
}

func qualify(personaName, key string) string { return personaName + "|" + key }
