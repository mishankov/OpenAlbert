package orchestration

import (
	"context"
	"errors"
	"time"

	"github.com/mishankov/OpenAlbert/shared/codemode"
	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

type retryPolicy struct {
	InitialInterval    time.Duration
	MaximumInterval    time.Duration
	BackoffCoefficient float64
	MaximumAttempts    int           // 0 = unlimited until ctx is done or non-retryable
	Timeout            time.Duration // per-attempt timeout; 0 = none
}

var (
	defaultActivityPolicy = retryPolicy{
		InitialInterval:    time.Second,
		MaximumInterval:    15 * time.Second,
		BackoffCoefficient: 2,
		MaximumAttempts:    4,
		Timeout:            3 * time.Minute,
	}
	sttActivityPolicy = retryPolicy{
		InitialInterval:    15 * time.Second,
		MaximumInterval:    time.Minute,
		BackoffCoefficient: 2,
		MaximumAttempts:    5,
		Timeout:            12 * time.Minute,
	}
	browserActivityPolicy = retryPolicy{
		InitialInterval:    5 * time.Second,
		MaximumInterval:    5 * time.Second,
		BackoffCoefficient: 1,
		MaximumAttempts:    0,
		Timeout:            2 * time.Minute,
	}
	cleanupActivityPolicy = retryPolicy{
		InitialInterval:    time.Second,
		MaximumInterval:    5 * time.Second,
		BackoffCoefficient: 2,
		MaximumAttempts:    3,
		Timeout:            30 * time.Second,
	}
)

type semaphore chan struct{}

func newSemaphore(n int) semaphore {
	if n <= 0 {
		return nil
	}
	return make(semaphore, n)
}

func (s semaphore) acquire(ctx context.Context) error {
	if s == nil {
		return nil
	}
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s semaphore) release() {
	if s == nil {
		return
	}
	<-s
}

func withSem(ctx context.Context, sem semaphore, fn func(context.Context) error) error {
	if err := sem.acquire(ctx); err != nil {
		return err
	}
	defer sem.release()
	return fn(ctx)
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// disconnectedCtx preserves telemetry values while detaching from parent
// cancellation so Finish, CloseSession, and failure receipts still persist.
func disconnectedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

func retryDelayForAttempt(policy retryPolicy, attempt int) time.Duration {
	delay := policy.InitialInterval
	if delay <= 0 {
		delay = time.Second
	}
	coeff := policy.BackoffCoefficient
	if coeff <= 0 {
		coeff = 2
	}
	for i := 1; i < attempt; i++ {
		next := time.Duration(float64(delay) * coeff)
		if next <= delay {
			if policy.MaximumInterval > 0 {
				return policy.MaximumInterval
			}
			return delay
		}
		delay = next
		if policy.MaximumInterval > 0 && delay >= policy.MaximumInterval {
			return policy.MaximumInterval
		}
	}
	if policy.MaximumInterval > 0 && delay > policy.MaximumInterval {
		return policy.MaximumInterval
	}
	return delay
}

func retryCall(ctx context.Context, clk Clock, policy retryPolicy, fn func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		attemptCtx := telemetry.WithFields(ctx, map[string]any{"attempt": attempt})
		var cancel context.CancelFunc
		if policy.Timeout > 0 {
			attemptCtx, cancel = context.WithTimeout(attemptCtx, policy.Timeout)
		}
		err := fn(attemptCtx)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			if isCanceled(err) {
				return err
			}
			return ctx.Err()
		}
		if domain.IsNonRetryable(err) {
			return err
		}
		if policy.MaximumAttempts > 0 && attempt >= policy.MaximumAttempts {
			return err
		}
		delay, ok := domain.RetryDelay(err)
		if !ok {
			delay = retryDelayForAttempt(policy, attempt)
		}
		if !sleepCtx(ctx, clk, delay) {
			return ctx.Err()
		}
	}
}

// activityHooks provides package-internal interception points for deterministic
// orchestration tests without adding overhead or public surface in production.
type activityHooks struct {
	onStart        func(name string, arg any)
	onComplete     func(name string, result any, err error)
	prepare        func(ctx context.Context, in agent.RunInput, next func(context.Context, agent.RunInput) (agent.Turn, error)) (agent.Turn, error)
	modelCall      func(ctx context.Context, in agent.ModelInput, next func(context.Context, agent.ModelInput) (agent.ModelOutput, error)) (agent.ModelOutput, error)
	toolCall       func(ctx context.Context, in agent.ToolInput, next func(context.Context, agent.ToolInput) (string, error)) (string, error)
	browserCall    func(ctx context.Context, lane string, in agent.ToolInput, next func(context.Context, agent.ToolInput) (string, error)) (string, error)
	transcribe     func(ctx context.Context, in agent.AudioInput, next func(context.Context, agent.AudioInput) (domain.AudioResult, error)) (domain.AudioResult, error)
	analyze        func(ctx context.Context, session domain.Session, next func(context.Context, domain.Session) error) error
	dispatchEv     func(convID string, ev Event) bool
	dispatchBackup func(in BackupEventInput) bool
	dispatchConsol func(in MemoryConsolidationInput) bool
}

func (r *Runtime) hookStart(name string, arg any) {
	if r.hooks != nil && r.hooks.onStart != nil {
		r.hooks.onStart(name, arg)
	}
}

func (r *Runtime) hookComplete(name string, result any, err error) {
	if r.hooks != nil && r.hooks.onComplete != nil {
		r.hooks.onComplete(name, result, err)
	}
}

func (r *Runtime) actPrepare(ctx context.Context, in agent.RunInput) (agent.Turn, error) {
	r.ensureInit()
	var out agent.Turn
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("Prepare", in)
		var e error
		if r.hooks != nil && r.hooks.prepare != nil {
			out, e = r.hooks.prepare(actCtx, in, r.Activities.Prepare)
		} else {
			out, e = r.Activities.Prepare(actCtx, in)
		}
		return e
	})
	r.hookComplete("Prepare", out, err)
	return out, err
}

func (r *Runtime) actPrepareAudio(ctx context.Context, in agent.RunInput) ([]agent.AudioInput, error) {
	r.ensureInit()
	var out []agent.AudioInput
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("PrepareAudio", in)
		var e error
		out, e = r.Activities.PrepareAudio(actCtx, in)
		return e
	})
	r.hookComplete("PrepareAudio", out, err)
	return out, err
}

func (r *Runtime) actTranscribeAudio(ctx context.Context, in agent.AudioInput) (domain.AudioResult, error) {
	r.ensureInit()
	var out domain.AudioResult
	err := retryCall(ctx, r.clock(), sttActivityPolicy, func(actCtx context.Context) error {
		return withSem(actCtx, r.sttSem, func(semCtx context.Context) error {
			r.hookStart("TranscribeAudio", in)
			var e error
			if r.hooks != nil && r.hooks.transcribe != nil {
				out, e = r.hooks.transcribe(semCtx, in, r.Activities.TranscribeAudio)
			} else {
				out, e = r.Activities.TranscribeAudio(semCtx, in)
			}
			return e
		})
	})
	r.hookComplete("TranscribeAudio", out, err)
	return out, err
}

func (r *Runtime) actRecordAudioFailure(ctx context.Context, in agent.AudioFailure) error {
	r.ensureInit()
	err := retryCall(ctx, r.clock(), cleanupActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("RecordAudioFailure", in)
		return r.Activities.RecordAudioFailure(actCtx, in)
	})
	r.hookComplete("RecordAudioFailure", nil, err)
	return err
}

func (r *Runtime) actModelCall(ctx context.Context, in agent.ModelInput) (agent.ModelOutput, error) {
	r.ensureInit()
	var out agent.ModelOutput
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		return withSem(actCtx, r.generalSem, func(semCtx context.Context) error {
			r.hookStart("ModelCall", in)
			var e error
			if r.hooks != nil && r.hooks.modelCall != nil {
				out, e = r.hooks.modelCall(semCtx, in, r.Activities.ModelCall)
			} else {
				out, e = r.Activities.ModelCall(semCtx, in)
			}
			return e
		})
	})
	r.hookComplete("ModelCall", out, err)
	return out, err
}

func (r *Runtime) actToolCall(ctx context.Context, in agent.ToolInput) (string, error) {
	r.ensureInit()
	var out string
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		return withSem(actCtx, r.generalSem, func(semCtx context.Context) error {
			r.hookStart("ToolCall", in)
			var e error
			if r.hooks != nil && r.hooks.toolCall != nil {
				out, e = r.hooks.toolCall(semCtx, in, r.Activities.ToolCall)
			} else {
				out, e = r.Activities.ToolCall(semCtx, in)
			}
			return e
		})
	})
	r.hookComplete("ToolCall", out, err)
	return out, err
}

func (r *Runtime) actBrowserToolCall(ctx context.Context, in agent.ToolInput) (string, error) {
	r.ensureInit()
	lane := ".browser-ui"
	sem := r.browserUISem
	if in.TaskID != "" || in.Background {
		lane = ".browser-bg"
		sem = r.browserBGSem
	}
	var out string
	err := retryCall(ctx, r.clock(), browserActivityPolicy, func(actCtx context.Context) error {
		return withSem(actCtx, sem, func(semCtx context.Context) error {
			r.hookStart("BrowserToolCall", in)
			var e error
			if r.hooks != nil && r.hooks.browserCall != nil {
				out, e = r.hooks.browserCall(semCtx, lane, in, r.Activities.BrowserToolCall)
			} else {
				out, e = r.Activities.BrowserToolCall(semCtx, in)
			}
			return e
		})
	})
	r.hookComplete("BrowserToolCall", out, err)
	return out, err
}

type persistMessageArg struct {
	SessionID string
	Key       string
	Message   domain.Message
}

func (r *Runtime) actPersistMessage(ctx context.Context, session, key string, msg domain.Message) error {
	r.ensureInit()
	arg := persistMessageArg{SessionID: session, Key: key, Message: msg}
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("PersistMessage", arg)
		return r.Activities.PersistMessage(actCtx, session, key, msg)
	})
	r.hookComplete("PersistMessage", nil, err)
	return err
}

func (r *Runtime) actArtifactView(ctx context.Context, scope, id string) (domain.Message, error) {
	r.ensureInit()
	var out domain.Message
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("ArtifactView", id)
		var e error
		out, e = r.Activities.ArtifactView(actCtx, scope, id)
		return e
	})
	r.hookComplete("ArtifactView", out, err)
	return out, err
}

func (r *Runtime) actFinish(ctx context.Context, in agent.FinishInput) error {
	r.ensureInit()
	err := retryCall(ctx, r.clock(), cleanupActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("Finish", in)
		return r.Activities.Finish(actCtx, in)
	})
	r.hookComplete("Finish", nil, err)
	return err
}

func (r *Runtime) actCloseSession(ctx context.Context, id string) error {
	r.ensureInit()
	err := retryCall(ctx, r.clock(), cleanupActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("CloseSession", id)
		return r.Activities.CloseSession(actCtx, id)
	})
	r.hookComplete("CloseSession", nil, err)
	return err
}

func (r *Runtime) actStartNewSession(ctx context.Context, scope, previous, next string, ids []string) (bool, error) {
	r.ensureInit()
	var started bool
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("StartNewSession", ids)
		var e error
		started, e = r.Activities.StartNewSession(actCtx, scope, previous, next, ids)
		return e
	})
	r.hookComplete("StartNewSession", started, err)
	return started, err
}

func (r *Runtime) actAcknowledgeStop(ctx context.Context, scope, session string, ids []string) error {
	r.ensureInit()
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("AcknowledgeStop", ids)
		return r.Activities.AcknowledgeStop(actCtx, scope, session, ids)
	})
	r.hookComplete("AcknowledgeStop", nil, err)
	return err
}

func (r *Runtime) actNotifyFailure(ctx context.Context, scope, key string) error {
	r.ensureInit()
	err := retryCall(ctx, r.clock(), cleanupActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("NotifyFailure", key)
		return r.Activities.NotifyFailure(actCtx, scope, key)
	})
	r.hookComplete("NotifyFailure", nil, err)
	return err
}

func (r *Runtime) actGetTask(ctx context.Context, scope, id string) (domain.Task, error) {
	r.ensureInit()
	var out domain.Task
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("GetTask", id)
		var e error
		out, e = r.Activities.GetTask(actCtx, scope, id)
		return e
	})
	r.hookComplete("GetTask", out, err)
	return out, err
}

func (r *Runtime) actBeginTask(ctx context.Context, scope, id string, now time.Time) (domain.Task, error) {
	r.ensureInit()
	var out domain.Task
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("BeginTask", id)
		var e error
		out, e = r.Activities.BeginTask(actCtx, scope, id, now)
		return e
	})
	r.hookComplete("BeginTask", out, err)
	return out, err
}

func (r *Runtime) actAdvanceTask(ctx context.Context, scope, id string, failed bool, now time.Time) (domain.Task, error) {
	r.ensureInit()
	var out domain.Task
	err := retryCall(ctx, r.clock(), cleanupActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("AdvanceTask", id)
		var e error
		out, e = r.Activities.AdvanceTask(actCtx, scope, id, failed, now)
		return e
	})
	r.hookComplete("AdvanceTask", out, err)
	return out, err
}

func (r *Runtime) actCodemodeStep(ctx context.Context, in agent.CodemodeInput) (codemode.Result, error) {
	r.ensureInit()
	var out codemode.Result
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		return withSem(actCtx, r.generalSem, func(semCtx context.Context) error {
			r.hookStart("CodemodeStep", in)
			var e error
			out, e = r.Activities.CodemodeStep(semCtx, in)
			return e
		})
	})
	r.hookComplete("CodemodeStep", out, err)
	return out, err
}

func (r *Runtime) actSaveCodemodeState(ctx context.Context, in agent.CodemodeState) error {
	r.ensureInit()
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("SaveCodemodeState", in)
		return r.Activities.SaveCodemodeState(actCtx, in)
	})
	r.hookComplete("SaveCodemodeState", nil, err)
	return err
}

func (r *Runtime) actAnalyze(ctx context.Context, session domain.Session) error {
	r.ensureInit()
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		return withSem(actCtx, r.analysisSem, func(semCtx context.Context) error {
			r.hookStart("Analyze", session)
			if r.hooks != nil && r.hooks.analyze != nil {
				return r.hooks.analyze(semCtx, session, r.Activities.Analyze)
			}
			return r.Activities.Analyze(semCtx, session)
		})
	})
	r.hookComplete("Analyze", nil, err)
	return err
}

func (r *Runtime) actBackupAlert(ctx context.Context, in agent.BackupAlertInput) error {
	r.ensureInit()
	err := retryCall(ctx, r.clock(), cleanupActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("BackupAlert", in)
		return r.Activities.BackupAlert(actCtx, in)
	})
	r.hookComplete("BackupAlert", nil, err)
	return err
}

func (r *Runtime) actPrepareMemoryConsolidation(ctx context.Context, in agent.ConsolidationRunInput) (agent.ConsolidationPrep, error) {
	r.ensureInit()
	var out agent.ConsolidationPrep
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("PrepareMemoryConsolidation", in)
		var e error
		out, e = r.Activities.PrepareMemoryConsolidation(actCtx, in)
		return e
	})
	r.hookComplete("PrepareMemoryConsolidation", out, err)
	return out, err
}

func (r *Runtime) actConsolidateProfileBatch(ctx context.Context, in agent.ConsolidationBatchInput) (agent.ConsolidationPhaseResult, error) {
	r.ensureInit()
	var out agent.ConsolidationPhaseResult
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		return withSem(actCtx, r.generalSem, func(semCtx context.Context) error {
			r.hookStart("ConsolidateProfileBatch", in)
			var e error
			out, e = r.Activities.ConsolidateProfileBatch(semCtx, in)
			return e
		})
	})
	r.hookComplete("ConsolidateProfileBatch", out, err)
	return out, err
}

func (r *Runtime) actConsolidateHygieneBatch(ctx context.Context, in agent.ConsolidationBatchInput) (agent.ConsolidationPhaseResult, error) {
	r.ensureInit()
	var out agent.ConsolidationPhaseResult
	err := retryCall(ctx, r.clock(), defaultActivityPolicy, func(actCtx context.Context) error {
		return withSem(actCtx, r.generalSem, func(semCtx context.Context) error {
			r.hookStart("ConsolidateHygieneBatch", in)
			var e error
			out, e = r.Activities.ConsolidateHygieneBatch(semCtx, in)
			return e
		})
	})
	r.hookComplete("ConsolidateHygieneBatch", out, err)
	return out, err
}

func (r *Runtime) actFinalizeMemoryConsolidation(ctx context.Context, in agent.ConsolidationFinalizeInput) (domain.MemoryConsolidationReport, error) {
	r.ensureInit()
	var out domain.MemoryConsolidationReport
	err := retryCall(ctx, r.clock(), cleanupActivityPolicy, func(actCtx context.Context) error {
		r.hookStart("FinalizeMemoryConsolidation", in)
		var e error
		out, e = r.Activities.FinalizeMemoryConsolidation(actCtx, in)
		return e
	})
	r.hookComplete("FinalizeMemoryConsolidation", out, err)
	return out, err
}
