package policy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudquery/cloudquery/internal/logging"
	"github.com/cloudquery/cloudquery/pkg/core/database"
	"github.com/cloudquery/cloudquery/pkg/core/state"
	sdkdb "github.com/cloudquery/cq-provider-sdk/database"
	"github.com/cloudquery/cq-provider-sdk/provider/diag"
	"github.com/cloudquery/cq-provider-sdk/provider/execution"
	"github.com/google/uuid"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/rs/zerolog/log"
)

const (
	CloudQueryOrg = "cloudquery-policies"
)

type LowLevelQueryExecer interface {
	execution.Copier
	execution.QueryExecer
}

// RunRequest is the request used to run one or more policy.
type RunRequest struct {
	// Policies to run
	Policies Policies
	// Directory to load / save policies to
	Directory string
	// OutputDir is the output dir for policy execution output.
	OutputDir string
	// RunCallback is the callback method that is called after every policy execution.
	RunCallback UpdateCallback
	// DBPersistence defines weather or not to store run results
	DBPersistence bool
}

type RunResponse struct {
	Policies   Policies
	Executions []*ExecutionResult
}

func Snapshot(ctx context.Context, sta *state.Client, storage database.Storage, policy *Policy, outputPath, subpath string) error {
	db, err := sdkdb.New(ctx, logging.NewZHcLog(&log.Logger, "executor-database"), storage.DSN())
	if err != nil {
		return err
	}
	e := NewExecutor(db, sta, nil)

	if err := e.createViews(ctx, policy); err != nil {
		return err
	}

	tableNames, err := e.extractTableNames(ctx, policy.Checks[0].Query)
	if err != nil {
		return err
	}
	snapShotPath, err := createSnapshotPath(outputPath, subpath)
	if err != nil {
		return err
	}
	err = StoreSnapshot(ctx, e, snapShotPath, tableNames)
	if err != nil {
		return err
	}

	return StoreOutput(ctx, e, policy, snapShotPath)
}
func Load(ctx context.Context, directory string, policy *Policy) (*Policy, diag.Diagnostics) {
	var dd diag.Diagnostics
	// if policy is configured with source we load it first
	if policy.Source != "" {
		log.Debug().Str("policy", policy.Name).Str("source", policy.Source).Msg("loading policy from source")
		policy, dd = loadPolicyFromSource(ctx, directory, policy.Name, policy.SubPolicy(), policy.Source)
		if dd.HasDiags() {
			return nil, dd
		}
	}
	// TODO: add recursive stop
	// load inner policies
	for i, p := range policy.Policies {
		log.Debug().Str("policy", policy.Name).Str("inner_policy", p.Name).Msg("loading inner policy from source")
		policy.Policies[i], dd = Load(ctx, directory, p)
		if dd.HasErrors() {
			return nil, dd
		}
	}
	return policy, nil
}

func Run(ctx context.Context, sta *state.Client, storage database.Storage, req *RunRequest) (*RunResponse, diag.Diagnostics) {
	var (
		diags diag.Diagnostics
		resp  = &RunResponse{
			Policies:   make(Policies, 0),
			Executions: make([]*ExecutionResult, 0),
		}
	)
	for _, p := range req.Policies {
		log.Info().Str("policy", p.Name).Str("version", p.Version()).Str("subPath", p.SubPolicy()).Msg("preparing to run policy")
		loadedPolicy, dd := Load(ctx, req.Directory, p)
		if dd != nil {
			return nil, diag.FromError(dd, diag.INTERNAL)
		}

		policyExecution := &state.PolicyExecution{
			Scheme:     p.SourceType(),
			Location:   p.Source,
			PolicyName: p.String(),
			Selector:   p.SubPolicy(),
			Sha256Hash: p.Sha256Hash(),
			Version:    p.Version(),
		}
		if req.DBPersistence {
			var err error
			if policyExecution, err = sta.CreatePolicyExecution(ctx, policyExecution); err != nil {
				return nil, diag.FromError(err, diag.DATABASE, diag.WithSummary("failed to create policy execution"))
			}
		}

		resp.Policies = append(resp.Policies, loadedPolicy)
		log.Debug().Str("policy", p.Name).Str("version", p.Version()).Str("subPath", p.SubPolicy()).Msg("loaded policy successfully")
		result, dd := run(ctx, sta, storage, &ExecuteRequest{
			Policy:          loadedPolicy,
			UpdateCallback:  req.RunCallback,
			PolicyExecution: policyExecution,
			DBPersistence:   req.DBPersistence,
		})

		diags = diags.Add(dd)
		if diags.HasErrors() {
			// this error means error in execution and not policy violation
			// we should exit immediately as this is a non-recoverable error
			// might mean schema is incorrect, provider version
			log.Error().Msg("policy execution finished with error")
			return resp, diags
		}
		log.Info().Str("policy", p.Name).Msg("policy execution finished")
		resp.Executions = append(resp.Executions, result)
		if req.OutputDir == "" {
			continue
		}
		log.Info().Str("policy", p.Name).Str("version", p.Version()).Str("subPath", p.SubPolicy()).Msg("writing policy to output directory")

		diags = diags.Add(GenerateExecutionResultFile(result, req.OutputDir))

		if diags.HasErrors() {
			log.Error().Msg("failed to generate execution result file")
			return nil, diags
		}
	}
	return resp, diags
}

func Prune(ctx context.Context, sta *state.Client, pruneBefore time.Time) diag.Diagnostics {
	if err := sta.PrunePolicyExecutions(ctx, pruneBefore); err != nil {
		return diag.FromError(err, diag.DATABASE, diag.WithSummary("failed to prune policy executions"))
	}
	return nil
}

func run(ctx context.Context, sta *state.Client, storage database.Storage, request *ExecuteRequest) (*ExecutionResult, diag.Diagnostics) {
	var (
		totalQueriesToRun = request.Policy.TotalQueries()
		finishedQueries   = 0
	)
	filteredPolicy := request.Policy.Filter(request.Policy.meta.SubPolicy)
	if !filteredPolicy.HasChecks() {
		log.Error().Str("selector", request.Policy.meta.SubPolicy).Strs("available_policies", filteredPolicy.Policies.All()).Msg("policy/query not found with provided sub-policy selector")
		return nil, diag.FromError(fmt.Errorf("%s//%s: %w", request.Policy.Name, request.Policy.meta.SubPolicy, ErrPolicyOrQueryNotFound),
			diag.USER, diag.WithDetails("%s//%s not found, run `cloudquery policy describe %s` to find all available policies", request.Policy.Name, request.Policy.meta.SubPolicy, request.Policy.Name))
	}
	totalQueriesToRun = filteredPolicy.TotalQueries()
	log.Info().Int("total", totalQueriesToRun).Msg("policy Checks count")
	// set the progress total queries to run
	if request.UpdateCallback != nil {
		request.UpdateCallback(Update{
			PolicyName:      request.Policy.Name,
			Source:          request.Policy.Source,
			Version:         request.Policy.meta.Version,
			FinishedQueries: 0,
			QueriesCount:    totalQueriesToRun,
			Error:           "",
		})
	}

	// replace console update function to keep track the current status
	var progressUpdate = func(update Update) {
		finishedQueries += update.FinishedQueries
		if request.UpdateCallback != nil {
			request.UpdateCallback(Update{
				PolicyName:      request.Policy.Name,
				Source:          request.Policy.Source,
				Version:         request.Policy.meta.Version,
				FinishedQueries: finishedQueries,
				QueriesCount:    totalQueriesToRun,
				Error:           "",
			})
		}
	}
	db, err := sdkdb.New(ctx, logging.NewZHcLog(&log.Logger, "executor-database"), storage.DSN())
	if err != nil {
		return nil, diag.FromError(err, diag.DATABASE)
	}
	// execute the queries
	return NewExecutor(db, sta, progressUpdate).Execute(ctx, request, &filteredPolicy, nil)
}

func loadPolicyFromSource(ctx context.Context, directory, name, subPolicy, sourceURL string) (*Policy, diag.Diagnostics) {
	data, meta, err := LoadSource(ctx, directory, sourceURL)
	if err != nil {
		return nil, diag.FromError(err, diag.INTERNAL)
	}
	f, dd := hclsyntax.ParseConfig(data, name, hcl.Pos{Byte: 0, Line: 1, Column: 1})
	if dd.HasErrors() {
		return nil, diag.FromError(dd, diag.USER)
	}
	policy, dd := decodePolicy(f.Body, meta.Directory)
	if dd.HasErrors() {
		return nil, diag.FromError(dd, diag.USER)
	}
	policy.meta = meta
	if subPolicy != "" {
		policy.meta.SubPolicy = subPolicy
	}
	policy.Source = sourceURL
	return policy, nil
}

func createSnapshotPath(directory, queryName string) (string, error) {
	path := strings.TrimSuffix(directory, "/")
	cleanedPath := filepath.Join(path, queryName, "tests", uuid.NewString())

	err := os.MkdirAll(cleanedPath, os.ModePerm)
	if err != nil {
		return "", err
	}
	return cleanedPath, nil
}
