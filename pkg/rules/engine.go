package rules

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"

	"github.com/TypeOneLabs/tellury/pkg/graph"
)

// Engine executes rules against a frozen graph with bounded parallelism.
type Engine struct {
	Workers  int
	FailFast bool
}

// Result carries findings plus per-rule diagnostics.
type Result struct {
	Findings []Finding
	Errors   map[string]error
	// Skipped[ruleID][SkipCode] = count.
	Skipped map[string]map[SkipCode]int
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (e Engine) Run(ctx context.Context, p *Pass, rs []Rule) (Result, error) {
	if !p.Graph.Frozen() {
		return Result{}, fmt.Errorf("rules: graph must be frozen before Run")
	}
	workers := e.Workers
	if workers <= 0 {
		workers = min(runtime.NumCPU(), max(len(rs), 1))
	}

	var (
		muRes sync.Mutex
		res   = Result{Errors: map[string]error{}, Skipped: map[string]map[SkipCode]int{}}
		wg    sync.WaitGroup
	)
	jobs := make(chan Rule)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				id := r.Meta().ID
				sub := *p
				sub.Log = p.Log.With("rule", id)
				sub.Skip = func(ruleID string, _ graph.Ref, code SkipCode) {
					muRes.Lock()
					m := res.Skipped[ruleID]
					if m == nil {
						m = map[SkipCode]int{}
						res.Skipped[ruleID] = m
					}
					m[code]++
					muRes.Unlock()
				}
				fs, err := r.Eval(ctx, &sub)

				muRes.Lock()
				if err != nil {
					res.Errors[id] = err
					if e.FailFast {
						cancel()
					}
				}
				for _, f := range fs {
					if f.RuleID == "" {
						f.RuleID = id
					}
					if f.Severity == "" {
						f.Severity = r.Meta().Severity
					}
					if f.Remediation == "" {
						f.Remediation = r.Meta().Remediation
					}
					res.Findings = append(res.Findings, f)
				}
				muRes.Unlock()
			}
		}()
	}
	for _, r := range rs {
		select {
		case jobs <- r:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()

	sort.SliceStable(res.Findings, func(i, j int) bool {
		a, b := res.Findings[i], res.Findings[j]
		switch {
		case a.MonthlyWasteUSD != b.MonthlyWasteUSD:
			return a.MonthlyWasteUSD > b.MonthlyWasteUSD
		case a.Resource != b.Resource:
			return a.Resource < b.Resource
		default:
			return a.RuleID < b.RuleID
		}
	})
	if e.FailFast && len(res.Errors) > 0 {
		for id, err := range res.Errors {
			return res, fmt.Errorf("rule %s: %w", id, err)
		}
	}
	return res, nil
}
