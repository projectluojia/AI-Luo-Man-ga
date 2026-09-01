package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

var (
	runIdentifierPattern = id.StableMixed
)

func (s *Store) CreateEchoRunIdempotentLimited(
	ctx context.Context,
	key string,
	fingerprint string,
	echo kernelecho.Record,
	run kernelecho.RunRecord,
	maxPending int,
) (_ string, _ bool, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "create_echo_run_idempotent", started, resultErr) }()
	if idempotency.ValidateKey(key) != nil || len(fingerprint) != 64 || maxPending < 0 || maxPending > 100000 {
		return "", false, idempotency.ErrInvalidRequest
	}
	if echo.AppID == "" || echo.ID == "" || echo.InputMessage == "" || echo.Status != kernelecho.StatusRunning || echo.CreatedAt.IsZero() {
		return "", false, kernelecho.ErrInvalidEchoRecord
	}
	if err := validateNewRun(echo, run); err != nil {
		return "", false, err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("begin idempotent Echo/Run creation: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "create idempotent Echo/Run")
	var existingFingerprint string
	var existingEchoID string
	err = tx.QueryRowContext(ctx, `SELECT request_fingerprint,echo_id FROM echo_create_requests WHERE app_id=? AND idempotency_key=?`, echo.AppID, key).Scan(&existingFingerprint, &existingEchoID)
	switch {
	case err == nil:
		if existingFingerprint != fingerprint {
			return "", false, idempotency.ErrKeyConflict
		}
		return existingEchoID, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, fmt.Errorf("read Echo creation idempotency record: %w", err)
	}
	if maxPending > 0 {
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE app_id=? AND status IN (?,?)`,
			echo.AppID, kernelecho.RunStatusQueued, kernelecho.RunStatusRunning,
		).Scan(&pending); err != nil {
			return "", false, fmt.Errorf("count pending Runs: %w", err)
		}
		if pending >= maxPending {
			return "", false, kernelecho.ErrQueueFull
		}
	}
	if err := insertEchoRun(ctx, tx, echo, run); err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO echo_create_requests(app_id,idempotency_key,request_fingerprint,echo_id,created_at) VALUES(?,?,?,?,?)`,
		echo.AppID, key, fingerprint, echo.ID, echo.CreatedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return "", false, fmt.Errorf("record Echo creation idempotency: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("commit idempotent Echo/Run creation: %w", err)
	}
	return echo.ID, true, nil
}

func insertEchoRun(ctx context.Context, tx *sql.Tx, echo kernelecho.Record, run kernelecho.RunRecord) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO echoes(echo_id,app_id,input_message,status,created_at,next_event_sequence) VALUES(?,?,?,?,?,1)`, echo.ID, echo.AppID, echo.InputMessage, echo.Status, echo.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("create echo: %w", err)
	}
	if err := insertRun(ctx, tx, run); err != nil {
		return err
	}
	return nil
}

func insertRun(ctx context.Context, tx *sql.Tx, run kernelecho.RunRecord) error {
	var parentRunID any
	if run.ParentRunID != "" {
		parentRunID = run.ParentRunID
	}
	capabilityScope, err := json.Marshal(nonNilStrings(run.CapabilityScope))
	if err != nil {
		return errors.Join(kernelecho.ErrInvalidRunRecord, err)
	}
	permissionScope, err := json.Marshal(nonNilStrings(run.PermissionScope))
	if err != nil {
		return errors.Join(kernelecho.ErrInvalidRunRecord, err)
	}
	contextSources := run.ContextSources
	if len(contextSources) == 0 {
		contextSources = json.RawMessage(`{}`) // 未装配前固化来源版本为空对象
	}
	resultPayload := run.Result.Data
	if resultPayload == nil {
		resultPayload = []byte{}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO runs(
	  app_id,run_id,run_group_id,echo_id,parent_run_id,origin_call_id,attempt,status,executor_id,config_revision,protocol_version,
  executor_config,input_payload,input_content_type,max_steps,max_capability_calls,max_execution_units,max_output_bytes,
  max_cost_microusd,execution_timeout_ms,used_execution_units,used_cost_microusd,used_retries,deadline_at,available_at,
  last_executor_sequence,capability_scope,permission_scope,recoverable_state,result_payload,result_content_type,created_at,
  session_id,user_id,message_id,context_digest,context_sources,channel
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.AppID, run.ID, run.RunGroupID, run.EchoID, parentRunID, run.OriginCallID, run.Attempt, run.Status, run.ExecutorID, run.ConfigRevision, run.ProtocolVersion,
		string(run.ExecutorConfig), run.InputPayload, run.InputContentType, run.MaxSteps, run.MaxCapabilityCalls, run.MaxExecutionUnits, run.MaxOutputBytes,
		run.MaxCostMicrousd, run.ExecutionTimeoutMS, run.UsedExecutionUnits, run.UsedCostMicrousd, run.UsedRetries,
		run.Deadline.UTC().Format(time.RFC3339Nano), run.AvailableAt.UTC().Format(time.RFC3339Nano), run.LastExecutorSequence,
		string(capabilityScope), string(permissionScope), string(run.RecoverableState), resultPayload, run.Result.ContentType,
		run.CreatedAt.UTC().Format(time.RFC3339Nano), run.SessionID, run.UserID, run.MessageID, run.ContextDigest, string(contextSources), run.Channel,
	); err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	return nil
}

func (s *Store) ClaimRun(ctx context.Context, appID, echoID, runID, leaseToken string, startedAt, leaseExpiresAt time.Time) (_ kernelecho.RunRecord, resultErr error) {
	metricStarted := time.Now()
	defer func() { observeStorageOperation(ctx, "claim_run", metricStarted, resultErr) }()
	if appID == "" || echoID == "" || runID == "" || leaseToken == "" || startedAt.IsZero() || !leaseExpiresAt.After(startedAt) {
		return kernelecho.RunRecord{}, kernelecho.ErrInvalidRunRecord
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return kernelecho.RunRecord{}, fmt.Errorf("begin run claim: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "claim Run")
	run, err := queryRun(tx.QueryRowContext(ctx, runSelect+` WHERE app_id=? AND echo_id=? AND run_id=? AND status=? AND julianday(available_at)<=julianday(?)`,
		appID, echoID, runID, kernelecho.RunStatusQueued, startedAt.UTC().Format(time.RFC3339Nano)))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return kernelecho.RunRecord{}, kernelecho.ErrInvalidTransition
		}
		return kernelecho.RunRecord{}, fmt.Errorf("select queued run: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,lease_token=?,lease_expires_at=?,started_at=? WHERE app_id=? AND run_id=? AND echo_id=? AND status=?`,
		kernelecho.RunStatusRunning, leaseToken, leaseExpiresAt.UTC().Format(time.RFC3339Nano), startedAt.UTC().Format(time.RFC3339Nano),
		appID, run.ID, echoID, kernelecho.RunStatusQueued,
	)
	if err != nil {
		return kernelecho.RunRecord{}, fmt.Errorf("claim queued run: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return kernelecho.RunRecord{}, fmt.Errorf("read claimed run row count: %w", err)
	}
	if affected != 1 {
		return kernelecho.RunRecord{}, kernelecho.ErrInvalidTransition
	}
	if err := tx.Commit(); err != nil {
		return kernelecho.RunRecord{}, fmt.Errorf("commit run claim: %w", err)
	}
	run.Status = kernelecho.RunStatusRunning
	run.LeaseToken = leaseToken
	run.StartedAt = timePointer(startedAt.UTC())
	run.LeaseExpiresAt = timePointer(leaseExpiresAt.UTC())
	return run, nil
}

func (s *Store) RenewRunLease(ctx context.Context, run kernelecho.RunRecord, renewedAt, leaseExpiresAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "renew_run_lease", started, resultErr) }()
	if run.AppID == "" || run.ID == "" || run.EchoID == "" || run.LeaseToken == "" ||
		run.Status != kernelecho.RunStatusRunning || renewedAt.IsZero() || !leaseExpiresAt.After(renewedAt) {
		return kernelecho.ErrInvalidRunRecord
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE runs SET lease_expires_at=?
WHERE app_id=? AND run_id=? AND echo_id=? AND status=? AND lease_token=? AND julianday(lease_expires_at)>=julianday(?)`,
		leaseExpiresAt.UTC().Format(time.RFC3339Nano),
		run.AppID, run.ID, run.EchoID, kernelecho.RunStatusRunning, run.LeaseToken,
		renewedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("renew Run lease: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read renewed Run lease row count: %w", err)
	}
	if affected != 1 {
		return kernelecho.ErrInvalidTransition
	}
	return nil
}

func (s *Store) AdvanceRunExecutorSequence(ctx context.Context, run kernelecho.RunRecord, sequence uint64) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "advance_run_sequence", started, resultErr) }()
	if run.AppID == "" || run.ID == "" || run.EchoID == "" || run.LeaseToken == "" ||
		run.Status != kernelecho.RunStatusRunning || sequence == 0 || sequence != run.LastExecutorSequence+1 {
		return kernelecho.ErrInvalidRunRecord
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE runs SET last_executor_sequence=?
WHERE app_id=? AND run_id=? AND echo_id=? AND status=? AND lease_token=? AND last_executor_sequence=?`,
		sequence, run.AppID, run.ID, run.EchoID, kernelecho.RunStatusRunning, run.LeaseToken, run.LastExecutorSequence,
	)
	if err != nil {
		return fmt.Errorf("advance Run executor sequence: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read Run executor sequence update count: %w", err)
	}
	if affected != 1 {
		return kernelecho.ErrInvalidTransition
	}
	return nil
}

func (s *Store) AdvanceRunExecutorSequenceWithUsage(
	ctx context.Context,
	run kernelecho.RunRecord,
	sequence uint64,
	executionUnits uint64,
	costMicrousd uint64,
	retries uint32,
) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "advance_run_usage", started, resultErr) }()
	if run.AppID == "" || run.ID == "" || run.EchoID == "" || run.LeaseToken == "" ||
		run.Status != kernelecho.RunStatusRunning || sequence == 0 || sequence != run.LastExecutorSequence+1 ||
		executionUnits < run.UsedExecutionUnits || costMicrousd < run.UsedCostMicrousd ||
		retries < run.UsedRetries || retries > 320 ||
		executionUnits > run.MaxExecutionUnits || (run.MaxCostMicrousd > 0 && costMicrousd > run.MaxCostMicrousd) {
		return kernelecho.ErrInvalidRunRecord
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE runs
SET last_executor_sequence=?,used_execution_units=?,used_cost_microusd=?,used_retries=?
WHERE app_id=? AND run_id=? AND echo_id=? AND status=? AND lease_token=?
	  AND last_executor_sequence=? AND used_execution_units=? AND used_cost_microusd=? AND used_retries=?`,
		sequence, executionUnits, costMicrousd, retries,
		run.AppID, run.ID, run.EchoID, kernelecho.RunStatusRunning, run.LeaseToken,
		run.LastExecutorSequence, run.UsedExecutionUnits, run.UsedCostMicrousd, run.UsedRetries,
	)
	if err != nil {
		return fmt.Errorf("advance Run executor sequence and usage: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read Run executor usage update count: %w", err)
	}
	if affected != 1 {
		return kernelecho.ErrInvalidTransition
	}
	return nil
}

func (s *Store) CancelQueuedRun(ctx context.Context, appID, echoID string, completedAt time.Time) (_ bool, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "cancel_queued_run", started, resultErr) }()
	if appID == "" || echoID == "" || completedAt.IsZero() {
		return false, kernelecho.ErrInvalidRunRecord
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin queued Run cancellation: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "cancel queued Run")
	public := publicerror.Echo("cancelled")
	result, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,error_code=?,error_message=?,completed_at=? WHERE app_id=? AND echo_id=? AND status=?`,
		kernelecho.RunStatusCancelled, public.Code, public.Message, completedAt.UTC().Format(time.RFC3339Nano),
		appID, echoID, kernelecho.RunStatusQueued,
	)
	if err != nil {
		return false, fmt.Errorf("cancel queued run: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read cancelled queued Run row count: %w", err)
	}
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE app_id=? AND echo_id=? AND status=?`, appID, echoID, kernelecho.RunStatusRunning).Scan(&running); err != nil {
		return false, fmt.Errorf("check running Run before cancellation: %w", err)
	}
	if affected == 0 && running > 0 {
		return false, nil
	}
	if affected == 0 {
		return false, kernelecho.ErrInvalidTransition
	}
	if running > 0 {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit queued Run cancellation: %w", err)
		}
		return true, nil
	}
	result, err = tx.ExecContext(ctx, `UPDATE echoes SET status=?,final_message='',error_code=?,error_message=?,completed_at=? WHERE app_id=? AND echo_id=? AND status=?`,
		kernelecho.StatusCancelled, public.Code, public.Message, completedAt.UTC().Format(time.RFC3339Nano),
		appID, echoID, kernelecho.StatusRunning,
	)
	if err != nil {
		return false, fmt.Errorf("cancel queued Run Echo: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return false, fmt.Errorf("read cancelled Echo row count: %w", err)
	} else if affected != 1 {
		return false, kernelecho.ErrInvalidTransition
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit queued Run cancellation: %w", err)
	}
	return true, nil
}

func (s *Store) CancelQueuedRuns(ctx context.Context, appID string, completedAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "cancel_queued_runs", started, resultErr) }()
	if appID == "" || completedAt.IsZero() {
		return kernelecho.ErrInvalidRunRecord
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin queued Runs cancellation: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "cancel queued Runs")
	public := publicerror.Echo("cancelled")
	if _, err := tx.ExecContext(ctx, `
UPDATE echoes
SET status=?,final_message='',error_code=?,error_message=?,completed_at=?
WHERE app_id=? AND status=?
  AND EXISTS (
    SELECT 1 FROM runs
    WHERE runs.app_id=echoes.app_id AND runs.echo_id=echoes.echo_id AND runs.status=?
  )
  AND NOT EXISTS (
    SELECT 1 FROM runs
    WHERE runs.app_id=echoes.app_id AND runs.echo_id=echoes.echo_id AND runs.status=?
  )`,
		kernelecho.StatusCancelled, public.Code, public.Message, completedAt.UTC().Format(time.RFC3339Nano),
		appID, kernelecho.StatusRunning, kernelecho.RunStatusQueued, kernelecho.RunStatusRunning); err != nil {
		return fmt.Errorf("cancel queued Echoes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE runs SET status=?,error_code=?,error_message=?,completed_at=?
WHERE app_id=? AND status=?`,
		kernelecho.RunStatusCancelled, public.Code, public.Message, completedAt.UTC().Format(time.RFC3339Nano),
		appID, kernelecho.RunStatusQueued); err != nil {
		return fmt.Errorf("cancel queued Runs: %w", err)
	}
	return tx.Commit()
}

func (s *Store) RetryRun(ctx context.Context, current, next kernelecho.RunRecord, failure publicerror.Error, completedAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "retry_run", started, resultErr) }()
	if current.AppID == "" || current.ID == "" || current.EchoID == "" || current.LeaseToken == "" || completedAt.IsZero() ||
		next.AppID != current.AppID || next.EchoID != current.EchoID || next.Attempt != current.Attempt+1 ||
		next.Status != kernelecho.RunStatusQueued || next.RunGroupID != current.RunGroupID {
		return kernelecho.ErrInvalidRunRecord
	}
	if err := validateNewRun(kernelecho.Record{
		ID: current.EchoID, AppID: current.AppID, InputMessage: "persisted",
		Status: kernelecho.StatusRunning, CreatedAt: next.CreatedAt,
	}, next); err != nil {
		return err
	}
	failure = publicerror.Echo(failure.Code)
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Run retry: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "retry Run")
	result, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,lease_token=NULL,lease_expires_at=NULL,error_code=?,error_message=?,completed_at=? WHERE app_id=? AND run_id=? AND echo_id=? AND status=? AND lease_token=?`,
		kernelecho.RunStatusFailed, failure.Code, failure.Message, completedAt.UTC().Format(time.RFC3339Nano),
		current.AppID, current.ID, current.EchoID, kernelecho.RunStatusRunning, current.LeaseToken,
	)
	if err != nil {
		return fmt.Errorf("fail retry source run: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read retry source Run row count: %w", err)
	} else if affected != 1 {
		return kernelecho.ErrInvalidTransition
	}
	if err := insertRun(ctx, tx, next); err != nil {
		return fmt.Errorf("create retry run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Run retry: %w", err)
	}
	return nil
}

func (s *Store) CompleteRun(ctx context.Context, run kernelecho.RunRecord, runStatus, echoStatus string, output kernelecho.Output, failure publicerror.Error, completedAt time.Time) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "complete_run", started, resultErr) }()
	if err := validateCompletion(run, runStatus, echoStatus, completedAt); err != nil {
		return err
	}
	if runStatus == kernelecho.RunStatusSucceeded {
		failure = publicerror.Error{}
	} else {
		failure = publicerror.Echo(failure.Code)
		output = kernelecho.Output{}
	}
	if output.Data == nil {
		output.Data = []byte{}
	}
	if runStatus == kernelecho.RunStatusSucceeded &&
		(len(output.Data) == 0 || len(output.Data) > executor.MaxOutputPayloadBytes || output.ContentType == "") {
		return kernelecho.ErrInvalidRunRecord
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Run/Echo completion: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "complete Run")
	result, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,lease_token=NULL,lease_expires_at=NULL,result_payload=?,result_content_type=?,error_code=?,error_message=?,completed_at=? WHERE app_id=? AND run_id=? AND echo_id=? AND status=? AND lease_token=?`,
		runStatus, output.Data, output.ContentType, failure.Code, failure.Message, completedAt.UTC().Format(time.RFC3339Nano),
		run.AppID, run.ID, run.EchoID, kernelecho.RunStatusRunning, run.LeaseToken,
	)
	if err != nil {
		return fmt.Errorf("complete run: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read completed run row count: %w", err)
	} else if affected != 1 {
		return kernelecho.ErrInvalidTransition
	}
	result, err = tx.ExecContext(ctx, `UPDATE echoes SET status=?,final_message=?,result_payload=?,result_content_type=?,error_code=?,error_message=?,completed_at=? WHERE app_id=? AND echo_id=? AND status=?`,
		echoStatus, textOutput(output), output.Data, output.ContentType, failure.Code, failure.Message, completedAt.UTC().Format(time.RFC3339Nano),
		run.AppID, run.EchoID, kernelecho.StatusRunning,
	)
	if err != nil {
		return fmt.Errorf("complete echo: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read completed echo row count: %w", err)
	} else if affected != 1 {
		return kernelecho.ErrInvalidTransition
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Run/Echo completion: %w", err)
	}
	return nil
}

func finalizeEchoFromRun(ctx context.Context, tx *sql.Tx, appID, echoID string, completedAt time.Time) error {
	var activeRuns int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE app_id=? AND echo_id=? AND status IN (?,?)`,
		appID, echoID, kernelecho.RunStatusQueued, kernelecho.RunStatusRunning,
	).Scan(&activeRuns); err != nil {
		return fmt.Errorf("check active Echo Runs: %w", err)
	}
	if activeRuns != 0 {
		return nil
	}
	var rootStatus, resultContentType, errorCode, errorMessage string
	var resultPayload []byte
	if err := tx.QueryRowContext(ctx, `
SELECT status,result_payload,result_content_type,coalesce(error_code,''),coalesce(error_message,'')
FROM runs WHERE app_id=? AND echo_id=?
ORDER BY attempt DESC LIMIT 1`, appID, echoID,
	).Scan(&rootStatus, &resultPayload, &resultContentType, &errorCode, &errorMessage); err != nil {
		return fmt.Errorf("read terminal Run state: %w", err)
	}
	echoStatus := kernelecho.StatusFailed
	if resultPayload == nil {
		resultPayload = []byte{}
	}
	switch rootStatus {
	case "":
	case kernelecho.RunStatusSucceeded:
		echoStatus = kernelecho.StatusSucceeded
		errorCode, errorMessage = "", ""
	case kernelecho.RunStatusCancelled:
		echoStatus = kernelecho.StatusCancelled
	case kernelecho.RunStatusFailed, kernelecho.RunStatusTimedOut:
	default:
		return kernelecho.ErrInvalidTransition
	}
	result, err := tx.ExecContext(ctx, `
UPDATE echoes SET status=?,final_message=?,result_payload=?,result_content_type=?,error_code=?,error_message=?,completed_at=?
WHERE app_id=? AND echo_id=? AND status=?`,
		echoStatus, textOutput(kernelecho.Output{ContentType: resultContentType, Data: resultPayload}), resultPayload, resultContentType, errorCode, errorMessage, completedAt.UTC().Format(time.RFC3339Nano),
		appID, echoID, kernelecho.StatusRunning,
	)
	if err != nil {
		return fmt.Errorf("finalize Echo from Run: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read finalized Echo row count: %w", err)
	} else if affected != 1 {
		return kernelecho.ErrInvalidTransition
	}
	return nil
}

func (s *Store) AppendEchoEvent(ctx context.Context, event kernelecho.Event) (_ kernelecho.Event, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "append_echo_event", started, resultErr) }()
	if event.AppID == "" || event.EchoID == "" || event.RunID == "" || event.Sequence != 0 || event.Type == "" || event.CreatedAt.IsZero() || !json.Valid(event.Payload) {
		return kernelecho.Event{}, kernelecho.ErrInvalidEchoEvent
	}
	// 已取消的上下文不要开事务：modernc/sqlite 在取消竞速时开事务会遗留
	// 写锁状态，导致后续写事务撞 busy_timeout 失败（事件在取消后本就不可用）。
	if err := ctx.Err(); err != nil {
		return kernelecho.Event{}, err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return kernelecho.Event{}, fmt.Errorf("begin Echo event append: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "append Echo event")
	var runExists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM runs r JOIN echoes e ON e.app_id=r.app_id AND e.echo_id=r.echo_id WHERE r.app_id=? AND r.echo_id=? AND r.run_id=? AND r.status=? AND e.status=?`,
		event.AppID, event.EchoID, event.RunID, kernelecho.RunStatusRunning, kernelecho.StatusRunning,
	).Scan(&runExists); err != nil {
		return kernelecho.Event{}, fmt.Errorf("validate Echo event run: %w", err)
	}
	if runExists != 1 {
		return kernelecho.Event{}, kernelecho.ErrInvalidTransition
	}
	if err := tx.QueryRowContext(ctx, `UPDATE echoes SET next_event_sequence=next_event_sequence+1 WHERE app_id=? AND echo_id=? RETURNING next_event_sequence-1`, event.AppID, event.EchoID).Scan(&event.Sequence); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return kernelecho.Event{}, kernelecho.ErrEchoNotFound
		}
		return kernelecho.Event{}, fmt.Errorf("allocate Echo event sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO echo_events(app_id,echo_id,sequence,type,payload,created_at,run_id) VALUES(?,?,?,?,?,?,?)`, event.AppID, event.EchoID, event.Sequence, event.Type, string(event.Payload), event.CreatedAt.UTC().Format(time.RFC3339Nano), event.RunID); err != nil {
		return kernelecho.Event{}, fmt.Errorf("append echo event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return kernelecho.Event{}, fmt.Errorf("commit Echo event append: %w", err)
	}
	return event, nil
}

func (s *Store) GetEcho(ctx context.Context, appID, echoID string) (_ kernelecho.Record, _ []kernelecho.Event, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "get_echo", started, resultErr) }()
	if appID == "" || echoID == "" {
		return kernelecho.Record{}, nil, kernelecho.ErrEchoNotFound
	}
	var echo kernelecho.Record
	var createdAt string
	var completedAt sql.NullString
	var resultPayload []byte
	var resultContentType string
	err := s.db.QueryRowContext(ctx, `SELECT echo_id,app_id,input_message,status,coalesce(final_message,''),coalesce(result_payload,''),coalesce(result_content_type,''),coalesce(error_code,''),coalesce(error_message,''),created_at,completed_at FROM echoes WHERE app_id=? AND echo_id=?`, appID, echoID).Scan(&echo.ID, &echo.AppID, &echo.InputMessage, &echo.Status, &echo.FinalMessage, &resultPayload, &resultContentType, &echo.ErrorCode, &echo.ErrorMessage, &createdAt, &completedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return kernelecho.Record{}, nil, kernelecho.ErrEchoNotFound
		}
		return kernelecho.Record{}, nil, fmt.Errorf("get echo: %w", err)
	}
	echo.Result = kernelecho.Output{ContentType: resultContentType, Data: append([]byte(nil), resultPayload...)}
	echo.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return kernelecho.Record{}, nil, fmt.Errorf("parse echo created_at: %w", err)
	}
	if completedAt.Valid {
		value, parseErr := time.Parse(time.RFC3339Nano, completedAt.String)
		if parseErr != nil {
			return kernelecho.Record{}, nil, fmt.Errorf("parse echo completed_at: %w", parseErr)
		}
		echo.CompletedAt = &value
	}
	rows, err := s.db.QueryContext(ctx, `SELECT app_id,echo_id,sequence,type,payload,created_at,run_id FROM echo_events WHERE app_id=? AND echo_id=? ORDER BY sequence`, appID, echoID)
	if err != nil {
		return kernelecho.Record{}, nil, fmt.Errorf("query echo events: %w", err)
	}
	defer rows.Close()
	events := []kernelecho.Event{}
	for rows.Next() {
		var event kernelecho.Event
		var payload string
		var timestamp string
		if err := rows.Scan(&event.AppID, &event.EchoID, &event.Sequence, &event.Type, &payload, &timestamp, &event.RunID); err != nil {
			return kernelecho.Record{}, nil, fmt.Errorf("scan echo event: %w", err)
		}
		event.Payload = []byte(payload)
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return kernelecho.Record{}, nil, fmt.Errorf("parse echo event time: %w", err)
		}
		events = append(events, event)
	}
	return echo, events, rows.Err()
}

func (s *Store) ListRuns(ctx context.Context, appID, echoID string) (_ []kernelecho.RunRecord, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_runs", started, resultErr) }()
	if appID == "" || echoID == "" {
		return nil, kernelecho.ErrInvalidRunRecord
	}
	rows, err := s.db.QueryContext(ctx, runSelect+` WHERE app_id=? AND echo_id=? ORDER BY created_at,run_id`, appID, echoID)
	if err != nil {
		return nil, fmt.Errorf("query Echo runs: %w", err)
	}
	defer rows.Close()
	runs := make([]kernelecho.RunRecord, 0)
	for rows.Next() {
		run, err := queryRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Echo run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Echo runs: %w", err)
	}
	return runs, nil
}

func (s *Store) GetRun(ctx context.Context, appID, runID string) (_ kernelecho.RunRecord, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "get_run", started, resultErr) }()
	if appID == "" || runID == "" {
		return kernelecho.RunRecord{}, kernelecho.ErrRunNotFound
	}
	run, err := queryRun(s.db.QueryRowContext(ctx, runSelect+` WHERE app_id=? AND run_id=?`, appID, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return kernelecho.RunRecord{}, kernelecho.ErrRunNotFound
	}
	if err != nil {
		return kernelecho.RunRecord{}, fmt.Errorf("get Run: %w", err)
	}
	return run, nil
}

func (s *Store) ListQueuedRuns(ctx context.Context, appID string, limit int) (_ []kernelecho.RunWork, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_queued_runs", started, resultErr) }()
	if appID == "" || limit < 1 || limit > 1000 {
		return nil, kernelecho.ErrInvalidRunRecord
	}
	rows, err := s.db.QueryContext(ctx, runSelect+` WHERE app_id=? AND status=? ORDER BY created_at,run_id LIMIT ?`, appID, kernelecho.RunStatusQueued, limit)
	if err != nil {
		return nil, fmt.Errorf("query queued runs: %w", err)
	}
	runs, err := scanRuns(rows, "queued")
	if err != nil {
		return nil, err
	}
	return s.loadRunWork(ctx, runs)
}

func scanRuns(rows *sql.Rows, kind string) ([]kernelecho.RunRecord, error) {
	runs := make([]kernelecho.RunRecord, 0)
	for rows.Next() {
		run, err := queryRun(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan %s run: %w", kind, err)
		}
		runs = append(runs, run)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close %s runs: %w", kind, err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s runs: %w", kind, err)
	}
	return runs, nil
}

func (s *Store) loadRunWork(ctx context.Context, runs []kernelecho.RunRecord) ([]kernelecho.RunWork, error) {
	work := make([]kernelecho.RunWork, 0, len(runs))
	now := time.Now().UTC()
	for _, run := range runs {
		if len(run.InputPayload) == 0 || run.InputContentType == "" {
			return nil, kernelecho.ErrInvalidRunRecord
		}
		var status string
		if err := s.db.QueryRowContext(ctx, `SELECT status FROM echoes WHERE app_id=? AND echo_id=? AND status=?`, run.AppID, run.EchoID, kernelecho.StatusRunning).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// Echo 已进入终态而 Run 仍排队（取消/崩溃窗口等边缘路径）：确定性
				// 失败该 Run，避免孤儿排队 Run 永久占用唯一槽位。
				if failErr := s.failOrphanQueuedRun(ctx, run, now); failErr != nil {
					observe.Warn(ctx, "孤儿排队 Run 确定性失败失败",
						observe.StringAttr("app_id", run.AppID),
						observe.StringAttr("echo_id", run.EchoID),
						observe.StringAttr("run_id", run.ID),
					)
				}
				continue
			}
			return nil, fmt.Errorf("read queued Run Echo input: %w", err)
		}
		work = append(work, kernelecho.RunWork{Run: run})
	}
	return work, nil
}

// failOrphanQueuedRun 把 Echo 已进入终态的排队 Run 确定性转移为失败
// （仅从 queued 转移，幂等；并发已转移时 RowsAffected 为 0）。
func (s *Store) failOrphanQueuedRun(ctx context.Context, run kernelecho.RunRecord, now time.Time) error {
	public := publicerror.Echo("recovery_failed")
	result, err := s.db.ExecContext(ctx, `
	UPDATE runs SET status=?,lease_token=NULL,lease_expires_at=NULL,result_payload='',result_content_type='',error_code=?,error_message=?,completed_at=?
WHERE app_id=? AND run_id=? AND status=?`,
		kernelecho.RunStatusFailed, public.Code, public.Message, now.Format(time.RFC3339Nano),
		run.AppID, run.ID, kernelecho.RunStatusQueued,
	)
	if err != nil {
		return fmt.Errorf("fail orphan queued run: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read orphan run update count: %w", err)
	}
	if affected == 1 {
		observe.Info(ctx, "孤儿排队 Run 已确定性失败（Echo 已进入终态）",
			observe.StringAttr("app_id", run.AppID),
			observe.StringAttr("echo_id", run.EchoID),
			observe.StringAttr("run_id", run.ID),
		)
	}
	return nil
}

func (s *Store) ListRunnableRuns(ctx context.Context, appID string, now time.Time, limit int) (_ []kernelecho.RunWork, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_runnable_runs", started, resultErr) }()
	if appID == "" || now.IsZero() || limit < 1 || limit > 1000 {
		return nil, kernelecho.ErrInvalidRunRecord
	}
	rows, err := s.db.QueryContext(ctx, runSelect+` WHERE app_id=? AND status=? AND julianday(available_at)<=julianday(?) ORDER BY julianday(available_at),julianday(created_at),run_id LIMIT ?`,
		appID, kernelecho.RunStatusQueued, now.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, fmt.Errorf("query runnable runs: %w", err)
	}
	runs, err := scanRuns(rows, "runnable")
	if err != nil {
		return nil, err
	}
	return s.loadRunWork(ctx, runs)
}

func (s *Store) FailAbandonedRuns(ctx context.Context, appID string, now time.Time) (_ int64, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "fail_abandoned_runs", started, resultErr) }()
	if appID == "" || now.IsZero() {
		return 0, kernelecho.ErrInvalidRunRecord
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin abandoned Run reconciliation: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "fail abandoned Runs")
	rows, err := tx.QueryContext(ctx, `
SELECT run_id,echo_id,status
FROM runs
WHERE app_id=? AND status=?
ORDER BY created_at,run_id`,
		appID, kernelecho.RunStatusRunning)
	if err != nil {
		return 0, fmt.Errorf("query abandoned runs: %w", err)
	}
	type identity struct {
		runID  string
		echoID string
		status string
	}
	identities := make([]identity, 0)
	for rows.Next() {
		var item identity
		if err := rows.Scan(&item.runID, &item.echoID, &item.status); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan abandoned run: %w", err)
		}
		identities = append(identities, item)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close abandoned runs: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate abandoned runs: %w", err)
	}
	public := publicerror.Echo("recovery_failed")
	for _, item := range identities {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,lease_token=NULL,lease_expires_at=NULL,result_payload='',result_content_type='',error_code=?,error_message=?,completed_at=? WHERE app_id=? AND run_id=? AND status=?`,
			kernelecho.RunStatusFailed, public.Code, public.Message, now.UTC().Format(time.RFC3339Nano), appID, item.runID, item.status,
		); err != nil {
			return 0, fmt.Errorf("fail abandoned run: %w", err)
		}
	}
	seenEchoes := make(map[string]struct{}, len(identities))
	for _, item := range identities {
		if _, seen := seenEchoes[item.echoID]; seen {
			continue
		}
		seenEchoes[item.echoID] = struct{}{}
		if err := finalizeEchoFromRun(ctx, tx, appID, item.echoID, now); err != nil {
			return 0, fmt.Errorf("reconcile abandoned Run Echo: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit abandoned Run reconciliation: %w", err)
	}
	return int64(len(identities)), nil
}

func (s *Store) RecordCapabilityCall(ctx context.Context, callID, runID, echoID, appID, capabilityID string, payload []byte, success bool, failure publicerror.Error, duration time.Duration) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "record_capability_audit", started, resultErr) }()
	if appID == "" || callID == "" || runID == "" || echoID == "" || capabilityID == "" || duration < 0 || !json.Valid(payload) {
		return kernelecho.ErrInvalidAuditRecord
	}
	if success {
		failure = publicerror.Error{}
	} else {
		failure = publicerror.NormalizeCapability(failure)
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO capability_audit(call_id,run_id,echo_id,app_id,capability_id,payload,success,error_code,error_message,duration_ms,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(app_id,run_id,call_id) DO NOTHING`,
		callID, runID, echoID, appID, capabilityID, string(payload), boolInt(success), failure.Code, failure.Message,
		duration.Milliseconds(), time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("record capability call: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read capability audit insert count: %w", err)
	}
	if affected == 1 {
		return nil
	}
	var storedEchoID string
	var storedCapabilityID string
	var storedPayload string
	var storedSuccess int
	var storedErrorCode string
	var storedErrorMessage string
	if err := s.db.QueryRowContext(ctx, `
SELECT echo_id,capability_id,payload,success,coalesce(error_code,''),coalesce(error_message,'')
	FROM capability_audit WHERE app_id=? AND run_id=? AND call_id=?`,
		appID, runID, callID,
	).Scan(&storedEchoID, &storedCapabilityID, &storedPayload, &storedSuccess, &storedErrorCode, &storedErrorMessage); err != nil {
		return fmt.Errorf("read duplicate capability audit: %w", err)
	}
	if storedEchoID != echoID || storedCapabilityID != capabilityID || storedPayload != string(payload) ||
		storedSuccess != boolInt(success) || storedErrorCode != failure.Code || storedErrorMessage != failure.Message {
		return idempotency.ErrKeyConflict
	}
	return nil
}

func (s *Store) ListCapabilityCalls(ctx context.Context, appID, echoID string) (_ []kernelecho.CapabilityAuditRecord, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_capability_audit", started, resultErr) }()
	if appID == "" || echoID == "" {
		return nil, kernelecho.ErrInvalidAuditRecord
	}
	rows, err := s.db.QueryContext(ctx, `SELECT app_id,run_id,call_id,echo_id,capability_id,payload,success,coalesce(error_code,''),coalesce(error_message,''),duration_ms,created_at FROM capability_audit WHERE app_id=? AND echo_id=? ORDER BY created_at,run_id,call_id`, appID, echoID)
	if err != nil {
		return nil, fmt.Errorf("query capability audit: %w", err)
	}
	defer rows.Close()
	records := make([]kernelecho.CapabilityAuditRecord, 0)
	for rows.Next() {
		var record kernelecho.CapabilityAuditRecord
		var payload string
		var success int
		var createdAt string
		if err := rows.Scan(&record.AppID, &record.RunID, &record.CallID, &record.EchoID, &record.CapabilityID, &payload, &success, &record.ErrorCode, &record.ErrorMessage, &record.DurationMS, &createdAt); err != nil {
			return nil, fmt.Errorf("scan capability audit: %w", err)
		}
		record.Payload = json.RawMessage(payload)
		record.Success = success == 1
		record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse capability audit time: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate capability audit: %w", err)
	}
	return records, nil
}

const runSelect = `
SELECT
  app_id,run_id,run_group_id,echo_id,coalesce(parent_run_id,''),origin_call_id,attempt,status,executor_id,config_revision,protocol_version,
  executor_config,input_payload,input_content_type,max_steps,max_capability_calls,max_execution_units,max_output_bytes,
  max_cost_microusd,execution_timeout_ms,used_execution_units,used_cost_microusd,used_retries,
  deadline_at,available_at,coalesce(lease_token,''),lease_expires_at,last_executor_sequence,
  capability_scope,permission_scope,recoverable_state,result_payload,result_content_type,
  coalesce(error_code,''),coalesce(error_message,''),created_at,started_at,completed_at,
  coalesce(session_id,''),coalesce(user_id,''),coalesce(message_id,''),coalesce(context_digest,''),coalesce(context_sources,'{}'),
  coalesce(channel,'')
FROM runs`

func queryRun(scanner rowScanner) (kernelecho.RunRecord, error) {
	var run kernelecho.RunRecord
	var attempt int64
	var maxSteps int64
	var maxToolCalls int64
	var maxExecutionUnits int64
	var maxOutputBytes int64
	var maxCostMicrousd int64
	var executionTimeoutMS int64
	var usedExecutionUnits int64
	var usedCostMicrousd int64
	var usedRetries int64
	var lastExecutorSequence int64
	var deadlineAt string
	var availableAt string
	var leaseExpiresAt sql.NullString
	var capabilityScope string
	var permissionScope string
	var recoverableState string
	var executorConfig []byte
	var inputPayload []byte
	var inputContentType string
	var resultPayload []byte
	var resultContentType string
	var createdAt string
	var startedAt sql.NullString
	var completedAt sql.NullString
	var sessionID string
	var userID string
	var messageID string
	var contextDigest string
	var contextSources string
	if err := scanner.Scan(
		&run.AppID, &run.ID, &run.RunGroupID, &run.EchoID, &run.ParentRunID, &run.OriginCallID, &attempt, &run.Status,
		&run.ExecutorID, &run.ConfigRevision, &run.ProtocolVersion,
		&executorConfig, &inputPayload, &inputContentType, &maxSteps, &maxToolCalls, &maxExecutionUnits, &maxOutputBytes,
		&maxCostMicrousd, &executionTimeoutMS, &usedExecutionUnits, &usedCostMicrousd, &usedRetries,
		&deadlineAt, &availableAt,
		&run.LeaseToken, &leaseExpiresAt, &lastExecutorSequence,
		&capabilityScope, &permissionScope, &recoverableState, &resultPayload, &resultContentType,
		&run.ErrorCode, &run.ErrorMessage, &createdAt, &startedAt, &completedAt,
		&sessionID, &userID, &messageID, &contextDigest, &contextSources,
		&run.Channel,
	); err != nil {
		return kernelecho.RunRecord{}, err
	}
	if attempt < 0 || maxSteps < 0 || maxToolCalls < 0 || maxExecutionUnits < 0 || maxOutputBytes < 0 || maxCostMicrousd < 0 || executionTimeoutMS < 0 ||
		usedExecutionUnits < 0 || usedCostMicrousd < 0 ||
		usedRetries < 0 || usedRetries > 320 ||
		lastExecutorSequence < 0 {
		return kernelecho.RunRecord{}, kernelecho.ErrInvalidRunRecord
	}
	run.Attempt = uint32(attempt)
	run.MaxSteps = uint32(maxSteps)
	run.MaxCapabilityCalls = uint32(maxToolCalls)
	run.MaxExecutionUnits = uint64(maxExecutionUnits)
	run.MaxOutputBytes = uint64(maxOutputBytes)
	run.MaxCostMicrousd = uint64(maxCostMicrousd)
	run.ExecutionTimeoutMS = uint32(executionTimeoutMS)
	run.UsedExecutionUnits = uint64(usedExecutionUnits)
	run.UsedCostMicrousd = uint64(usedCostMicrousd)
	run.UsedRetries = uint32(usedRetries)
	run.LastExecutorSequence = uint64(lastExecutorSequence)
	run.RecoverableState = json.RawMessage(recoverableState)
	run.SessionID = sessionID
	run.UserID = userID
	run.MessageID = messageID
	run.ContextDigest = contextDigest
	run.ContextSources = json.RawMessage(contextSources)
	run.ExecutorConfig = json.RawMessage(executorConfig)
	run.InputPayload = append([]byte(nil), inputPayload...)
	run.InputContentType = inputContentType
	run.Result = kernelecho.Output{ContentType: resultContentType, Data: append([]byte(nil), resultPayload...)}
	if len(capabilityScope) > 65536 || len(permissionScope) > 65536 ||
		json.Unmarshal([]byte(capabilityScope), &run.CapabilityScope) != nil ||
		json.Unmarshal([]byte(permissionScope), &run.PermissionScope) != nil ||
		!validCanonicalScope(run.CapabilityScope, executor.MaxCapabilities, capability.IsStableID) ||
		!validCanonicalScope(run.PermissionScope, 256, id.IsPermission) {
		return kernelecho.RunRecord{}, kernelecho.ErrInvalidRunRecord
	}
	var err error
	if run.Deadline, err = time.Parse(time.RFC3339Nano, deadlineAt); err != nil {
		return kernelecho.RunRecord{}, fmt.Errorf("parse Run deadline: %w", err)
	}
	if run.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return kernelecho.RunRecord{}, fmt.Errorf("parse Run creation time: %w", err)
	}
	if run.AvailableAt, err = time.Parse(time.RFC3339Nano, availableAt); err != nil {
		return kernelecho.RunRecord{}, fmt.Errorf("parse Run availability time: %w", err)
	}
	if run.LeaseExpiresAt, err = parseOptionalTime(leaseExpiresAt); err != nil {
		return kernelecho.RunRecord{}, fmt.Errorf("parse Run lease expiry: %w", err)
	}
	if run.StartedAt, err = parseOptionalTime(startedAt); err != nil {
		return kernelecho.RunRecord{}, fmt.Errorf("parse Run start time: %w", err)
	}
	if run.CompletedAt, err = parseOptionalTime(completedAt); err != nil {
		return kernelecho.RunRecord{}, fmt.Errorf("parse Run completion time: %w", err)
	}
	return run, nil
}

func parseOptionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func validateNewRun(echo kernelecho.Record, run kernelecho.RunRecord) error {
	if run.AppID == "" || run.ID == "" || run.RunGroupID == "" || run.EchoID == "" ||
		!runIdentifierPattern.MatchString(run.ID) || !runIdentifierPattern.MatchString(run.RunGroupID) ||
		run.AppID != echo.AppID || run.EchoID != echo.ID ||
		run.Attempt == 0 || run.Status != kernelecho.RunStatusQueued || run.ExecutorID == "" || run.ConfigRevision == "" ||
		run.ProtocolVersion == "" || run.MaxSteps == 0 || run.MaxSteps > executor.MaxProtocolSteps ||
		run.MaxCapabilityCalls == 0 || run.MaxCapabilityCalls > executor.MaxCapabilityCalls ||
		run.MaxExecutionUnits == 0 || run.MaxExecutionUnits > executor.MaxExecutionUnits ||
		run.MaxOutputBytes == 0 || run.MaxOutputBytes > executor.MaxOutputPayloadBytes ||
		run.MaxCostMicrousd > executor.MaxCostMicrousd ||
		run.MaxExecutionUnits > math.MaxInt64 || run.MaxOutputBytes > math.MaxInt64 ||
		run.MaxCostMicrousd > math.MaxInt64 ||
		run.ExecutionTimeoutMS < 100 || run.ExecutionTimeoutMS > uint32((5*time.Minute).Milliseconds()) ||
		run.UsedExecutionUnits != 0 || run.UsedCostMicrousd != 0 ||
		run.UsedRetries != 0 ||
		run.CreatedAt.IsZero() || run.AvailableAt.IsZero() || run.AvailableAt.Before(run.CreatedAt) || !run.Deadline.After(run.AvailableAt) ||
		run.LastExecutorSequence != 0 || run.LeaseToken != "" || run.LeaseExpiresAt != nil || run.StartedAt != nil ||
		run.CompletedAt != nil || len(run.Result.Data) != 0 || run.Result.ContentType != "" || run.ErrorCode != "" || run.ErrorMessage != "" ||
		len(run.ExecutorConfig) == 0 || len(run.ExecutorConfig) > executor.MaxExecutorConfigBytes || !json.Valid(run.ExecutorConfig) ||
		len(run.InputPayload) == 0 || len(run.InputPayload) > executor.MaxInputPayloadBytes || !validContentType(run.InputContentType) ||
		!json.Valid(run.RecoverableState) ||
		(run.SessionID != "" && !session.ValidStableID(run.SessionID)) ||
		(run.UserID != "" && !session.ValidStableID(run.UserID)) ||
		(run.MessageID != "" && !session.ValidStableID(run.MessageID)) ||
		run.ContextDigest != "" || // 上下文在执行开始时由 SetRunContext 一次性固化
		(len(run.ContextSources) != 0 && !json.Valid(run.ContextSources)) ||
		!validCanonicalScope(run.CapabilityScope, executor.MaxCapabilities, capability.IsStableID) ||
		!validCanonicalScope(run.PermissionScope, 256, id.IsPermission) {
		return kernelecho.ErrInvalidRunRecord
	}
	if (run.ParentRunID != "" && !runIdentifierPattern.MatchString(run.ParentRunID)) ||
		(run.OriginCallID != "" && !runIdentifierPattern.MatchString(run.OriginCallID)) {
		return kernelecho.ErrInvalidRunRecord
	}
	return nil
}

func validContentType(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func textOutput(output kernelecho.Output) string {
	if !strings.HasPrefix(strings.ToLower(output.ContentType), "text/plain") || !utf8.Valid(output.Data) {
		return ""
	}
	return string(output.Data)
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func validCanonicalScope(values []string, maximum int, valid func(string) bool) bool {
	if len(values) > maximum || !sort.StringsAreSorted(values) {
		return false
	}
	for index, value := range values {
		if !valid(value) || (index > 0 && values[index-1] == value) {
			return false
		}
	}
	return true
}

func isUniqueConstraint(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == 19
}

func validateCompletion(run kernelecho.RunRecord, runStatus, echoStatus string, completedAt time.Time) error {
	if run.AppID == "" || run.ID == "" || run.EchoID == "" || run.LeaseToken == "" || completedAt.IsZero() {
		return kernelecho.ErrInvalidRunRecord
	}
	valid := (runStatus == kernelecho.RunStatusSucceeded && echoStatus == kernelecho.StatusSucceeded) ||
		(runStatus == kernelecho.RunStatusFailed && echoStatus == kernelecho.StatusFailed) ||
		(runStatus == kernelecho.RunStatusTimedOut && echoStatus == kernelecho.StatusFailed) ||
		(runStatus == kernelecho.RunStatusCancelled && echoStatus == kernelecho.StatusCancelled)
	if !valid {
		return kernelecho.ErrInvalidTransition
	}
	return nil
}
