package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"sort"
	"time"
)

// ModelOption is one selectable model offered by the create form.
type ModelOption struct {
	Selector string   `json:"selector"`           // provider/id → omp --model
	Name     string   `json:"name"`               // display name
	Provider string   `json:"provider"`           // optgroup grouping
	Thinking []string `json:"thinking,omitempty"` // supported --thinking efforts
}

// ompModelsJSON mirrors the shape of `omp models --json`.
type ompModelsJSON struct {
	Models []struct {
		Provider string   `json:"provider"`
		Selector string   `json:"selector"`
		Name     string   `json:"name"`
		Thinking []string `json:"thinking"`
	} `json:"models"`
}

const modelsTTL = 5 * time.Minute

// modelOptions returns the create-form model list. When Config.Models is set it
// acts as a curated allowlist (enriched with names/thinking from omp when a
// selector matches); otherwise every auth-resolvable omp model is offered.
func (s *Server) modelOptions() []ModelOption {
	all := s.enumerateModels()
	if len(s.cfg.Models) == 0 {
		return all
	}
	bySel := make(map[string]ModelOption, len(all))
	for _, o := range all {
		bySel[o.Selector] = o
	}
	out := make([]ModelOption, 0, len(s.cfg.Models))
	for _, sel := range s.cfg.Models {
		if o, ok := bySel[sel]; ok {
			out = append(out, o)
		} else {
			out = append(out, ModelOption{Selector: sel, Name: sel, Provider: "configured"})
		}
	}
	return out
}

// enumerateModels runs `omp models --json` (auth-filtered) and caches the parsed
// list for modelsTTL. On failure it returns the last good cache (possibly nil),
// so a transient omp hiccup leaves the form on its previous list.
func (s *Server) enumerateModels() []ModelOption {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	if s.models != nil && time.Since(s.modelsAt) < modelsTTL {
		return s.models
	}
	omp := s.cfg.OmpBin
	if omp == "" {
		omp = "omp"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, omp, "models", "--json").Output() // #nosec G204 -- omp is operator-configured
	if err != nil {
		return s.models
	}
	var parsed ompModelsJSON
	if err := json.Unmarshal(out, &parsed); err != nil {
		return s.models
	}
	opts := make([]ModelOption, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		if m.Selector == "" {
			continue
		}
		opts = append(opts, ModelOption{
			Selector: m.Selector,
			Name:     m.Name,
			Provider: m.Provider,
			Thinking: m.Thinking,
		})
	}
	sort.Slice(opts, func(i, j int) bool {
		if opts[i].Provider != opts[j].Provider {
			return opts[i].Provider < opts[j].Provider
		}
		return opts[i].Name < opts[j].Name
	})
	s.models = opts
	s.modelsAt = time.Now()
	return opts
}

// handleModels serves the auth-resolvable model list for the create form.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.modelOptions())
}
