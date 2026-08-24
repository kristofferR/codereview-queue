package crq

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

type InitResult struct {
	GateRepo       string `json:"gate_repo"`
	DashboardIssue int    `json:"dashboard_issue"`
	CalibrationPR  int    `json:"calibration_pr,omitempty"`
	StateRef       string `json:"state_ref"`
}

func Init(ctx context.Context, cfg Config, gh *ghapi.GitHub, store StateStore) (InitResult, error) {
	if cfg.GateRepo == "" {
		return InitResult{}, errors.New("CRQ_REPO is required for init")
	}
	exists, err := gh.RepoExists(ctx, cfg.GateRepo)
	if err != nil {
		return InitResult{}, fmt.Errorf("checking gate repo %s: %w", cfg.GateRepo, err)
	}
	if !exists {
		return InitResult{}, fmt.Errorf("gate repo %s does not exist; create it first, then rerun crq init", cfg.GateRepo)
	}
	if cfg.CalibrationPR <= 0 {
		pr, err := ensureCalibrationPR(ctx, cfg, gh)
		if err != nil {
			return InitResult{}, err
		}
		cfg.CalibrationPR = pr
	}
	state, err := store.Update(ctx, func(st *State) error { return nil })
	if err != nil {
		return InitResult{}, err
	}
	if cfg.DashboardIssue <= 0 {
		body, err := issueBody(state, cfg)
		if err != nil {
			return InitResult{}, err
		}
		issue, err := gh.CreateIssue(ctx, cfg.GateRepo, renderTitle(state, cfg), body)
		if err != nil {
			return InitResult{}, err
		}
		cfg.DashboardIssue = issue.Number
	} else if err := store.SyncDashboard(ctx, state); err != nil {
		return InitResult{}, err
	}
	return InitResult{
		GateRepo:       cfg.GateRepo,
		DashboardIssue: cfg.DashboardIssue,
		CalibrationPR:  cfg.CalibrationPR,
		StateRef:       cfg.StateRef,
	}, nil
}

func ensureCalibrationPR(ctx context.Context, cfg Config, gh *ghapi.GitHub) (int, error) {
	owner := ownerOf(cfg.GateRepo)
	branch := "crq/calibration"
	query := url.Values{}
	query.Set("state", "open")
	query.Set("head", owner+":"+branch)
	pulls, err := gh.ListPulls(ctx, cfg.GateRepo, query)
	if err != nil {
		return 0, err
	}
	if len(pulls) > 0 {
		return pulls[0].Number, nil
	}
	repo, err := gh.GetRepo(ctx, cfg.GateRepo)
	if err != nil {
		return 0, err
	}
	baseSHA, err := gh.GetRef(ctx, cfg.GateRepo, repo.DefaultBranch)
	if err != nil {
		return 0, err
	}
	baseCommit, err := gh.GetCommit(ctx, cfg.GateRepo, baseSHA)
	if err != nil {
		return 0, err
	}
	body := []byte("# crq calibration\n\nThis draft PR exists so crq can ask CodeRabbit for account-wide rate-limit state without spending a review.\n")
	blob, err := gh.CreateBlob(ctx, cfg.GateRepo, body)
	if err != nil {
		return 0, err
	}
	tree, err := gh.CreateTree(ctx, cfg.GateRepo, baseCommit.Tree.SHA, []map[string]any{
		{"path": "CALIBRATION.md", "mode": "100644", "type": "blob", "sha": blob},
	})
	if err != nil {
		return 0, err
	}
	commit, err := gh.CreateCommit(ctx, cfg.GateRepo, "crq: calibration thread", tree, []string{baseSHA})
	if err != nil {
		return 0, err
	}
	if err := gh.CreateRef(ctx, cfg.GateRepo, branch, commit); err != nil {
		if updateErr := gh.UpdateRef(ctx, cfg.GateRepo, branch, commit, true); updateErr != nil {
			return 0, fmt.Errorf("create calibration ref: %w; update fallback: %w", err, updateErr)
		}
	}
	pr, err := gh.CreatePull(ctx, cfg.GateRepo, repo.DefaultBranch, branch, "crq calibration (do not merge)", "crq posts `"+cfg.RateLimitCommand+"` here to read account-wide CodeRabbit quota. Keep this PR open.", true)
	if err != nil {
		return 0, err
	}
	return pr.Number, nil
}
