package jobaccounting

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	cstables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/framework/modelcatalog"
	"github.com/maximhq/bifrost/framework/modelcatalog/datasheet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeVideoRetriever struct {
	resp  *schemas.BifrostVideoGenerationResponse
	err   error
	calls int
}

func (f *fakeVideoRetriever) RetrieveVideo(ctx context.Context, job *cstables.TableProviderJob) (*schemas.BifrostVideoGenerationResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// videoJob builds a submitted video job row carrying the dimensions the request
// asked for — the state recordVideoJobLifecycle leaves behind.
func videoJob(t *testing.T, videoID string, dims modelcatalog.VideoPricingDimensions) *cstables.TableProviderJob {
	t.Helper()
	params, err := MarshalVideoDimensions(dims)
	require.NoError(t, err)
	return &cstables.TableProviderJob{
		ID:               cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.OpenAI), videoID),
		Kind:             cstables.ProviderJobKindVideo,
		Provider:         string(schemas.OpenAI),
		JobID:            videoID,
		Model:            dims.Model,
		Params:           params,
		AccountingStatus: cstables.ProviderJobAccountingStatusPending,
	}
}

func generationDims(model, size string, seconds int) modelcatalog.VideoPricingDimensions {
	return modelcatalog.VideoPricingDimensions{
		Model:       model,
		RequestType: schemas.VideoGenerationRequest,
		Seconds:     &seconds,
		Size:        size,
	}
}

func settleVideo(t *testing.T, job *cstables.TableProviderJob, resp *schemas.BifrostVideoGenerationResponse) *Settlement {
	t.Helper()
	settlement, err := NewVideoSettler(nil).Settle(context.Background(), fakePricing{}, JobRequest{
		Provider:      schemas.OpenAI,
		ProviderJobID: job.JobID,
		FallbackModel: job.Model,
		Job:           job,
		Payload:       VideoPayload{Response: resp},
	})
	require.NoError(t, err)
	require.NotNil(t, settlement)
	return settlement
}

func TestVideoDimensionsRoundTripThroughJobParams(t *testing.T) {
	audio := true
	dims := generationDims("sora-2-pro", "1920x1080", 8)
	dims.Audio = &audio
	dims.Extra = map[string]any{"sample_count": float64(2)}

	got := VideoDimensionsFromJob(videoJob(t, "vid_round", dims))
	assert.Equal(t, "sora-2-pro", got.Model)
	assert.Equal(t, "1920x1080", got.Size)
	require.NotNil(t, got.Seconds)
	assert.Equal(t, 8, *got.Seconds)
	require.NotNil(t, got.Audio)
	assert.True(t, *got.Audio)
	assert.Equal(t, float64(2), got.Extra["sample_count"],
		"the research hatch must survive the round trip even though nothing prices on it yet")
}

func TestVideoDimensionsFromJob_MalformedParamsDoNotBlockSettlement(t *testing.T) {
	// A params blob we cannot read must not strand the job: the response may still
	// carry enough to price it.
	garbage := "{not json"
	job := &cstables.TableProviderJob{Kind: cstables.ProviderJobKindVideo, Params: &garbage}
	assert.Equal(t, modelcatalog.VideoPricingDimensions{}, VideoDimensionsFromJob(job))
}

func TestVideoSettle_PricesFromCapturedParamsWhenResponseIsSilent(t *testing.T) {
	// The common provider: a retrieve that reports a status and nothing else.
	job := videoJob(t, "vid_silent", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	})

	assert.True(t, settlement.Priced)
	assert.True(t, settlement.Complete)
	assert.InDelta(t, 8*0.70, settlement.Cost, 1e-12,
		"the submitted request is the only place the duration and size still exist")
	// The settlement is named for the read that produced it, mirroring batch_results:
	// two rows both reading video_generation read as two generations. The creating
	// request type moves to the debug blob, which is what repricing reads.
	assert.Equal(t, schemas.VideoRetrieveRequest, settlement.Object)
}

func TestVideoSettle_ResponseOverridesCapturedParams(t *testing.T) {
	// The provider downscaled: the bill follows what was produced, not what was asked.
	job := videoJob(t, "vid_downscaled", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status:  schemas.VideoStatusCompleted,
		Seconds: bifrost.Ptr("4"),
		Size:    "1280x720",
		Videos:  []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	})

	require.True(t, settlement.Priced)
	assert.InDelta(t, 4*0.30, settlement.Cost, 1e-12)
}

func TestVideoSettle_BillsEveryReturnedClip(t *testing.T) {
	job := videoJob(t, "vid_multi", generationDims("sora-2-pro", "1280x720", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{
			{Type: schemas.VideoOutputTypeURL},
			{Type: schemas.VideoOutputTypeURL},
			{Type: schemas.VideoOutputTypeURL},
		},
	})

	require.True(t, settlement.Priced)
	assert.InDelta(t, 3*8*0.30, settlement.Cost, 1e-12)
}

func TestVideoSettle_ProviderReportedCostWins(t *testing.T) {
	job := videoJob(t, "vid_provider_cost", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
		Usage:  &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0.42}},
	})

	require.True(t, settlement.Priced)
	assert.InDelta(t, 0.42, settlement.Cost, 1e-12,
		"a provider that reports its own figure is never overridden by an estimate")
}

// A failed generation is a real price of zero, not an absence of one. Recording it
// as unpriceable would leave free work parked in the sweeper forever.
func TestVideoSettle_FailedJobIsPricedAtZero(t *testing.T) {
	job := videoJob(t, "vid_failed", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusFailed,
		Error:  &schemas.VideoCreateError{Code: "generation_failed"},
	})

	assert.True(t, settlement.Priced, "failure is a final answer about the money")
	assert.True(t, settlement.Complete)
	assert.Zero(t, settlement.Cost)
	assert.Empty(t, settlement.UnpriceableReason)
}

// Content filtering is the one case we refuse to guess: real compute was spent, and
// whether the provider bills for it is undocumented. Parking it keeps the job
// re-drivable instead of over- or under-charging.
func TestVideoSettle_ContentFilteredParksRatherThanGuesses(t *testing.T) {
	job := videoJob(t, "vid_filtered", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status:        schemas.VideoStatusFailed,
		ContentFilter: &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{"safety"}},
	})

	assert.False(t, settlement.Priced)
	assert.Equal(t, UnpriceableReasonContentFiltered, settlement.UnpriceableReason)
	assert.Zero(t, settlement.Cost)
}

func TestVideoSettle_NoRateRecordsUnpricedRatherThanFree(t *testing.T) {
	job := videoJob(t, "vid_norate", generationDims("some-unknown-model", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	})

	assert.False(t, settlement.Priced, "no rate must never read as free")
	assert.True(t, settlement.RecordUnpriced, "the row must stay visible so backfill can reprice it")
	assert.Equal(t, UnpriceableReasonMissingVideoPricing, settlement.UnpriceableReason)
	assert.Equal(t, "some-unknown-model", settlement.UnpricedModel)
}

func TestVideoPoll_InProgressJobIsNotSettleable(t *testing.T) {
	job := videoJob(t, "vid_running", generationDims("sora-2-pro", "1920x1080", 8))
	retriever := &fakeVideoRetriever{resp: &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_running",
		Status: schemas.VideoStatusInProgress,
	}}

	poll, err := NewVideoSettler(retriever).Poll(context.Background(), job)
	require.NoError(t, err)
	require.NotNil(t, poll)
	assert.False(t, poll.Terminal)
	assert.False(t, poll.Settleable)
	require.NotNil(t, poll.Job)
	assert.Equal(t, string(schemas.VideoStatusInProgress), poll.Job.ProviderStatus)
}

func TestVideoPoll_MismatchedIDIsAnError(t *testing.T) {
	job := videoJob(t, "vid_expected", generationDims("sora-2-pro", "1920x1080", 8))
	retriever := &fakeVideoRetriever{resp: &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_someone_elses",
		Status: schemas.VideoStatusCompleted,
	}}

	_, err := NewVideoSettler(retriever).Poll(context.Background(), job)
	require.Error(t, err, "settling another job's response would bill the wrong request")
}

func TestVideoPoll_RetrieveErrorReschedules(t *testing.T) {
	job := videoJob(t, "vid_err", generationDims("sora-2-pro", "1920x1080", 8))
	retriever := &fakeVideoRetriever{err: errors.New("upstream unavailable")}

	_, err := NewVideoSettler(retriever).Poll(context.Background(), job)
	require.Error(t, err)
}

// Video jobs finish in minutes; the batch ladder would leave a finished one
// unsettled for a quarter of an hour.
func TestVideoBackoffStaysInMinutes(t *testing.T) {
	settler := NewVideoSettler(nil)
	base := time.Minute
	assert.Equal(t, time.Minute, settler.Backoff(1, base))
	assert.Equal(t, 2*time.Minute, settler.Backoff(20, base))
	assert.Equal(t, 5*time.Minute, settler.Backoff(40, base))
	assert.Less(t, settler.Backoff(40, base), NewBatchSettler(nil).Backoff(40, base))
}

func TestVideoSettlerSupportsOnlyRealVideoProviders(t *testing.T) {
	settler := NewVideoSettler(nil)
	for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.Gemini, schemas.Vertex, schemas.Replicate, schemas.Runway, schemas.Runware, schemas.Fal, schemas.Fastgen, schemas.MiniMax, schemas.BytePlus} {
		assert.True(t, settler.SupportsProvider(provider), string(provider))
	}
	// Every provider implements the VideoGeneration method, so a real allowlist is
	// the only thing keeping the sweeper from burning attempts on providers that
	// merely return "unsupported".
	assert.False(t, settler.SupportsProvider(schemas.Anthropic))
	assert.False(t, settler.SupportsProvider(schemas.Cohere))
}

// The video namespace must never collide with batch: the id is both the aggregate
// write's idempotency key and the governance dedupe key.
func TestVideoAccountingLogIDIsDistinctFromBatch(t *testing.T) {
	video := AccountingLogID(ProviderJobKindVideo, schemas.OpenAI, "same_id")
	batch := AccountingLogID(ProviderJobKindBatch, schemas.OpenAI, "same_id")
	assert.NotEqual(t, video, batch)
	assert.Equal(t, video, AccountingLogID(ProviderJobKindVideo, schemas.OpenAI, "same_id"), "the id must be deterministic")
}

// End to end through the shared engine: the sweeper picks up a due video job,
// settles it exactly once, and reports it exactly once.
func TestVideoSweeper_SettlesCompletedJobOnce(t *testing.T) {
	store := newFakeAccountingStore()
	due := time.Now().UTC().Add(-time.Minute)
	job := videoJob(t, "vid_sweep", generationDims("sora-2-pro", "1920x1080", 8))
	job.NextCheckAt = &due
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	// A batch job sitting in the same table must be left entirely alone.
	batchDue := due
	require.NoError(t, store.UpsertProviderJob(context.Background(), &cstables.TableProviderJob{
		ID:               cstables.ProviderJobID(cstables.ProviderJobKindBatch, string(schemas.OpenAI), "batch_untouched"),
		Kind:             cstables.ProviderJobKindBatch,
		Provider:         string(schemas.OpenAI),
		JobID:            "batch_untouched",
		AccountingStatus: cstables.ProviderJobAccountingStatusPending,
		NextCheckAt:      &batchDue,
	}))

	retriever := &fakeVideoRetriever{resp: &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_sweep",
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	}}
	reporter := &fakeUsageReporter{}
	sweeper := NewVideoSweeper(store, store, fakePricing{}, retriever, nil, reporter, SweeperConfig{
		Provider: schemas.OpenAI,
		Limit:    10,
	})

	sweeper.SweepOnce(context.Background())

	assert.Equal(t, 1, retriever.calls, "the video sweeper must not poll the batch row")
	settled := store.jobs[cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.OpenAI), "vid_sweep")]
	require.NotNil(t, settled)
	assert.Equal(t, cstables.ProviderJobAccountingStatusAccounted, settled.AccountingStatus)

	logID := AccountingLogID(ProviderJobKindVideo, schemas.OpenAI, "vid_sweep")
	entry := store.logs[logID]
	require.NotNil(t, entry, "the aggregate cost row must be keyed by the video namespace")
	require.NotNil(t, entry.Cost)
	assert.InDelta(t, 8*0.70, *entry.Cost, 1e-12)
	assert.Equal(t, string(schemas.VideoRetrieveRequest), entry.Object)
	require.NotNil(t, entry.VideoDebugParsed)
	require.NotNil(t, entry.VideoDebugParsed.Accounting)
	assert.Equal(t, schemas.VideoGenerationRequest, entry.VideoDebugParsed.Accounting.RequestType,
		"Object no longer names the rates this priced at, so repricing reads the creating type from here")
	require.Len(t, reporter.reports, 1)
	assert.Equal(t, logID, reporter.reports[0].RequestID)

	untouched := store.jobs[cstables.ProviderJobID(cstables.ProviderJobKindBatch, string(schemas.OpenAI), "batch_untouched")]
	require.NotNil(t, untouched)
	assert.Equal(t, cstables.ProviderJobAccountingStatusPending, untouched.AccountingStatus,
		"a batch row must never be settled by the video sweeper")

	// Settlement is at-least-once by design, so a second sweep must be inert.
	sweeper.SweepOnce(context.Background())
	assert.Len(t, store.logs, 1)
	assert.Len(t, reporter.reports, 1, "governance must be told exactly once")
}

// --- Inline settlement on the retrieve path ---

func accountVideoInline(t *testing.T, store *fakeAccountingStore, reporter *fakeUsageReporter, videoID string, resp *schemas.BifrostVideoGenerationResponse) *Outcome {
	t.Helper()
	out, err := AccountVideoJob(context.Background(), store, store, fakePricing{}, JobRequest{
		Provider:      schemas.OpenAI,
		ProviderJobID: videoID,
		FallbackModel: "sora-2-pro",
		UsageReporter: reporter,
		ClaimedBy:     "logging-node",
		Payload:       VideoPayload{Response: resp},
	})
	require.NoError(t, err)
	return out
}

// A user polling GET /v1/videos/{id} has already fetched the terminal response.
// Settling from it saves a provider call and a full sweep interval of delay.
func TestAccountVideoJob_SettlesFromTheCallersTerminalResponse(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}
	job := videoJob(t, "vid_inline", generationDims("sora-2-pro", "1920x1080", 8))
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	out := accountVideoInline(t, store, reporter, "vid_inline", &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_inline",
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	})

	require.NotNil(t, out)
	assert.True(t, out.Accounted)
	assert.InDelta(t, 8*0.70, out.Cost, 1e-12)

	settled := store.jobs[cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.OpenAI), "vid_inline")]
	require.NotNil(t, settled)
	assert.Equal(t, cstables.ProviderJobAccountingStatusAccounted, settled.AccountingStatus)
	assert.Len(t, store.logs, 1)
	require.Len(t, reporter.reports, 1)
}

// A video submitted before this build was charged at submission. Settling it now,
// off a row we never wrote, would bill it a second time.
func TestAccountVideoJob_RefusesAJobWeNeverSawSubmitted(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}

	out := accountVideoInline(t, store, reporter, "vid_pre_upgrade", &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_pre_upgrade",
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	})

	assert.Nil(t, out, "no coordination row means we never saw the submission")
	assert.Empty(t, store.logs, "nothing may be billed for a job we did not record")
	assert.Empty(t, reporter.reports)
}

// Clients poll a running job repeatedly; only a terminal response is settlement
// input.
func TestAccountVideoJob_IgnoresAStillRunningJob(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}
	job := videoJob(t, "vid_running_inline", generationDims("sora-2-pro", "1920x1080", 8))
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	for _, status := range []schemas.VideoStatus{schemas.VideoStatusQueued, schemas.VideoStatusInProgress} {
		out := accountVideoInline(t, store, reporter, "vid_running_inline", &schemas.BifrostVideoGenerationResponse{
			ID:     "vid_running_inline",
			Status: status,
		})
		assert.Nil(t, out, string(status))
	}
	assert.Empty(t, store.logs)
	assert.Empty(t, reporter.reports)
}

// Polling is not billing. A client that keeps calling retrieve after the job
// finished must not be charged again on every call.
func TestAccountVideoJob_RepeatedRetrievesBillOnce(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}
	job := videoJob(t, "vid_repoll", generationDims("sora-2-pro", "1280x720", 8))
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	resp := &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_repoll",
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	}
	first := accountVideoInline(t, store, reporter, "vid_repoll", resp)
	require.NotNil(t, first)
	assert.True(t, first.Accounted)

	for i := 0; i < 3; i++ {
		again := accountVideoInline(t, store, reporter, "vid_repoll", resp)
		require.NotNil(t, again)
		assert.False(t, again.Claimed, "a settled job must not be re-claimed")
		// The caller still wants a price to display, mirrored off the written row.
		assert.InDelta(t, first.Cost, again.Cost, 1e-12)
	}
	assert.Len(t, store.logs, 1)
	assert.Len(t, reporter.reports, 1, "governance must be told exactly once")
}

// The Runware case: the retrieve the caller already holds carries the provider's
// own figure, so inline settlement prices exactly without any datasheet rate.
func TestAccountVideoJob_UsesProviderReportedCostFromTheRetrieve(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}
	// No duration and an unknown model: nothing here is priceable from the catalog.
	job := videoJob(t, "vid_runware", modelcatalog.VideoPricingDimensions{
		Model:       "runware:tripo-v3.1",
		RequestType: schemas.VideoGenerationRequest,
	})
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	out := accountVideoInline(t, store, reporter, "vid_runware", &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_runware",
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
		Usage:  &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0.4}},
	})

	require.NotNil(t, out)
	assert.True(t, out.Accounted)
	assert.InDelta(t, 0.4, out.Cost, 1e-12,
		"a provider-reported cost settles a job the catalog cannot price at all")
}

// Settling inline and settling from the sweeper must land on the same row, or the
// two paths would each write their own cost record for one video.
func TestAccountVideoJob_SharesTheSweepersAggregateRow(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}
	due := time.Now().UTC().Add(-time.Minute)
	job := videoJob(t, "vid_both", generationDims("sora-2-pro", "1280x720", 8))
	job.NextCheckAt = &due
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	resp := &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_both",
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	}
	require.NotNil(t, accountVideoInline(t, store, reporter, "vid_both", resp))

	retriever := &fakeVideoRetriever{resp: resp}
	NewVideoSweeper(store, store, fakePricing{}, retriever, nil, reporter, SweeperConfig{
		Provider: schemas.OpenAI,
		Limit:    10,
	}).SweepOnce(context.Background())

	assert.Len(t, store.logs, 1, "one video, one cost row, whichever path got there first")
	assert.Len(t, reporter.reports, 1)
	assert.Equal(t, 0, retriever.calls, "an accounted job is no longer due, so the sweeper skips it")
}

// --- Unreachable jobs (the retry-loop fix) ---

func providerErr(status int, message string) error {
	return NewProviderCallError(&schemas.BifrostError{
		StatusCode: &status,
		Error:      &schemas.ErrorField{Message: message},
	})
}

// The three status families a provider actually returns for a job it will not
// serve, taken from real responses: Runway and OpenAI 404, Gemini 403.
func TestClassifyProviderCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want ProviderCallOutcome
	}{
		{"runway 404", providerErr(404, "Could not find Task"), ProviderCallGone},
		{"openai 404", providerErr(404, "Video with id 'video_x' not found."), ProviderCallGone},
		{"gone", providerErr(410, "gone"), ProviderCallGone},
		{"gemini 403", providerErr(403, "You do not have permission to access the Operation"), ProviderCallAccessDenied},
		{"unauthorized", providerErr(401, "invalid api key"), ProviderCallAccessDenied},
		{"billing", providerErr(402, "payment required"), ProviderCallAccessDenied},
		{"bad request", providerErr(400, "malformed id"), ProviderCallRejected},
		{"unprocessable", providerErr(422, "unprocessable"), ProviderCallRejected},
		// The two 4xx that mean "ask again", not "no".
		{"rate limited", providerErr(429, "slow down"), ProviderCallRetry},
		{"request timeout", providerErr(408, "timeout"), ProviderCallRetry},
		{"server error", providerErr(503, "unavailable"), ProviderCallRetry},
		{"anthropic overloaded", providerErr(529, "overloaded"), ProviderCallRetry},
		// No response at all must never be mistaken for a client error.
		{"transport failure", errors.New("dial tcp: connection refused"), ProviderCallRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifyProviderCall(tc.err))
		})
	}
}

// A provider that has forgotten the job must stop the polling on the first attempt.
// Rescheduling instead spends the whole 120-attempt budget over hours and then parks
// the job under max_poll_attempts, which names the wrong cause.
func TestVideoPoll_UnreachableJobIsTerminalOnTheFirstAttempt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"gone", providerErr(404, "Could not find Task"), UnpriceableReasonVideoGone},
		{"access denied", providerErr(403, "PERMISSION_DENIED"), UnpriceableReasonVideoAccessDenied},
		{"rejected", providerErr(400, "malformed id"), UnpriceableReasonVideoRequestRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := videoJob(t, "vid_unreachable", generationDims("sora-2-pro", "1920x1080", 8))
			poll, err := NewVideoSettler(&fakeVideoRetriever{err: tc.err}).Poll(context.Background(), job)

			require.NoError(t, err, "an unreachable job is an outcome, not a poll failure")
			require.NotNil(t, poll)
			assert.True(t, poll.Terminal)
			assert.False(t, poll.Settleable)
			assert.Equal(t, tc.reason, poll.UnpriceableReason)
			assert.Nil(t, poll.Job, "a failed call learned nothing worth persisting")
		})
	}
}

// A transient failure keeps the existing reschedule path.
func TestVideoPoll_TransientFailureStillRetries(t *testing.T) {
	job := videoJob(t, "vid_transient", generationDims("sora-2-pro", "1920x1080", 8))
	for _, err := range []error{providerErr(503, "unavailable"), providerErr(429, "slow down"), errors.New("timeout")} {
		poll, pollErr := NewVideoSettler(&fakeVideoRetriever{err: err}).Poll(context.Background(), job)
		require.Error(t, pollErr, "%v must stay a retryable poll failure", err)
		assert.Nil(t, poll)
	}
}

// End to end: the sweeper parks a gone video after exactly one provider call.
func TestVideoSweeper_GoneJobStopsPollingImmediately(t *testing.T) {
	store := newFakeAccountingStore()
	due := time.Now().UTC().Add(-time.Minute)
	job := videoJob(t, "vid_gone", generationDims("sora-2-pro", "1920x1080", 8))
	job.NextCheckAt = &due
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	retriever := &fakeVideoRetriever{err: providerErr(404, "Video with id 'vid_gone' not found.")}
	sweeper := NewVideoSweeper(store, store, fakePricing{}, retriever, nil, nil, SweeperConfig{
		Provider: schemas.OpenAI,
		Limit:    10,
	})

	sweeper.SweepOnce(context.Background())
	sweeper.SweepOnce(context.Background())

	assert.Equal(t, 1, retriever.calls,
		"a job the provider has forgotten must not be polled again")
	settled := store.jobs[cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.OpenAI), "vid_gone")]
	require.NotNil(t, settled)
	assert.Equal(t, cstables.ProviderJobAccountingStatusUnpriceable, settled.AccountingStatus)
	require.NotNil(t, settled.UnpriceableReason)
	assert.Equal(t, UnpriceableReasonVideoGone, *settled.UnpriceableReason)
	assert.Empty(t, store.logs, "an unreachable job bills nothing")
}

// --- Aggregate row detail ---

// A settled video writes its cost on its own row, minutes after the submission —
// same object, same model, differing only by a cost. The accounting block is what
// tells the two apart, so it must be present on the settlement and absent from the
// submission.
func TestVideoSettle_AggregateRowCarriesAccountingDetail(t *testing.T) {
	job := videoJob(t, "vid_debug", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	})
	require.NotNil(t, settlement.ApplyDebug)

	entry := &logstore.Log{}
	settlement.ApplyDebug(entry)
	require.NotNil(t, entry.VideoDebugParsed)
	assert.Equal(t, "vid_debug", entry.VideoDebugParsed.VideoID)
	assert.Equal(t, schemas.VideoStatusCompleted, entry.VideoDebugParsed.Status)

	acct := entry.VideoDebugParsed.Accounting
	require.NotNil(t, acct, "the accounting block is what marks this as the settlement row")
	require.NotNil(t, acct.Seconds)
	assert.Equal(t, 8, *acct.Seconds)
	assert.Equal(t, "1920x1080", acct.Size)
	assert.Equal(t, 1, acct.OutputCount)
	assert.False(t, acct.Incomplete)
}

// An unpriceable row is exactly the one an operator needs the detail on, so it must
// carry the same block — marked incomplete.
func TestVideoSettle_UnpriceableRowStillCarriesDetail(t *testing.T) {
	job := videoJob(t, "vid_norate_debug", generationDims("some-unknown-model", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	})

	require.False(t, settlement.Priced)
	require.NotNil(t, settlement.ApplyDebug)
	entry := &logstore.Log{}
	settlement.ApplyDebug(entry)
	require.NotNil(t, entry.VideoDebugParsed.Accounting)
	assert.True(t, entry.VideoDebugParsed.Accounting.Incomplete)
}

// HydrateFromLog is the inverse of the closure: a caller that lost the settlement
// claim reads the status back off the already-written row.
func TestVideoHydrateFromLog(t *testing.T) {
	entry := &logstore.Log{VideoDebugParsed: &schemas.BifrostVideoDebug{
		VideoID: "vid_x",
		Status:  schemas.VideoStatusCompleted,
	}}
	out := &Outcome{}
	NewVideoSettler(nil).HydrateFromLog(entry, out)
	assert.Equal(t, string(schemas.VideoStatusCompleted), out.Status)

	// A row written before video detail existed must not panic or invent a status.
	bare := &Outcome{}
	NewVideoSettler(nil).HydrateFromLog(&logstore.Log{}, bare)
	assert.Empty(t, bare.Status)
}

func nativeVideoJob(t *testing.T, provider schemas.ModelProvider, id string, dims modelcatalog.VideoPricingDimensions) *cstables.TableProviderJob {
	t.Helper()
	job := videoJob(t, id, dims)
	job.Provider = string(provider)
	job.ID = cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(provider), id)
	job.SelectedKeyID = "creating-key"
	return job
}

func TestVideoSettle_FalRequiresActualBilling(t *testing.T) {
	for _, status := range []schemas.VideoStatus{schemas.VideoStatusCompleted, schemas.VideoStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			job := nativeVideoJob(t, schemas.Fal, "fal-receipt", generationDims("sora-2-pro", "1920x1080", 8))
			resp := &schemas.BifrostVideoGenerationResponse{ID: job.JobID, Status: status, Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}}}
			_, err := NewVideoSettler(nil).Settle(context.Background(), fakePricing{}, JobRequest{Provider: schemas.Fal, ProviderJobID: job.JobID, Job: job, Payload: VideoPayload{Response: resp}})
			require.Error(t, err, "a catalog estimate or a failed status is not an actual Fal bill")
		})
	}
}

func TestVideoSettle_FalActualBillingIncludesZero(t *testing.T) {
	for _, status := range []schemas.VideoStatus{schemas.VideoStatusCompleted, schemas.VideoStatusFailed} {
		for _, cost := range []float64{0, 0.42} {
			t.Run(fmt.Sprintf("%s/%g", status, cost), func(t *testing.T) {
				job := nativeVideoJob(t, schemas.Fal, "fal-actual", generationDims("sora-2-pro", "1920x1080", 8))
				resp := &schemas.BifrostVideoGenerationResponse{ID: job.JobID, Status: status, Usage: &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: cost}}}
				settlement, err := NewVideoSettler(nil).Settle(context.Background(), fakePricing{}, JobRequest{Provider: schemas.Fal, ProviderJobID: job.JobID, Job: job, Payload: VideoPayload{Response: resp}})
				require.NoError(t, err)
				require.True(t, settlement.Priced)
				require.True(t, settlement.Complete)
				assert.Equal(t, cost, settlement.Cost)
				entry := &logstore.Log{}
				settlement.ApplyDebug(entry)
				require.NotNil(t, entry.VideoDebugParsed.Accounting.ProviderCost)
				assert.Equal(t, cost, *entry.VideoDebugParsed.Accounting.ProviderCost)
			})
		}
	}
}

type ignoreVideoProviderCostPricing struct{ fakePricing }

func (ignoreVideoProviderCostPricing) IsProviderCostIgnored(schemas.ModelProvider) bool { return true }

func (ignoreVideoProviderCostPricing) CalculateVideoCostDetails(dims modelcatalog.VideoPricingDimensions, provider schemas.ModelProvider, scopes *modelcatalog.PricingLookupScopes) modelcatalog.VideoCostDetails {
	dims.ProviderCost = nil
	return (fakePricing{}).CalculateVideoCostDetails(dims, provider, scopes)
}

func TestVideoSettle_FalIgnoreProviderCostUsesCatalog(t *testing.T) {
	job := nativeVideoJob(t, schemas.Fal, "fal-ignore", generationDims("sora-2-pro", "1920x1080", 8))
	for _, usage := range []*schemas.VideoUsage{nil, {Cost: &schemas.BifrostCost{TotalCost: 0.42}}} {
		resp := &schemas.BifrostVideoGenerationResponse{ID: job.JobID, Status: schemas.VideoStatusCompleted, Usage: usage, Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}}}
		settlement, err := NewVideoSettler(nil).Settle(context.Background(), ignoreVideoProviderCostPricing{}, JobRequest{Provider: schemas.Fal, ProviderJobID: job.JobID, Job: job, Payload: VideoPayload{Response: resp}})
		require.NoError(t, err)
		require.True(t, settlement.Priced)
		assert.InDelta(t, 8*0.70, settlement.Cost, 1e-12)
	}
}

func TestVideoSettle_FastgenFailedIsUnknownRatherThanFree(t *testing.T) {
	for _, provider := range []schemas.ModelProvider{schemas.Fastgen, "custom-video-provider"} {
		t.Run(string(provider), func(t *testing.T) {
			job := nativeVideoJob(t, provider, "native-failed", generationDims("sora-2-pro", "1920x1080", 8))
			settlement, err := NewVideoSettler(nil).Settle(context.Background(), fakePricing{}, JobRequest{Provider: provider, ProviderJobID: job.JobID, Job: job, Payload: VideoPayload{Response: &schemas.BifrostVideoGenerationResponse{Status: schemas.VideoStatusFailed}}})
			require.NoError(t, err)
			assert.False(t, settlement.Priced)
			assert.True(t, settlement.RecordUnpriced)
			assert.NotEmpty(t, settlement.UnpriceableReason)
		})
	}
}

func TestVideoSettle_FastgenNeedsNativeRateAndDimensions(t *testing.T) {
	ds := datasheet.New(nil, bifrost.NewNoOpLogger(), datasheet.Config{})
	provider := string(schemas.Fastgen)
	require.NoError(t, ds.SetOverrides([]cstables.TablePricingOverride{{ID: "fastgen-rate", Name: "native USD rate", ScopeKind: "provider", ProviderID: &provider, MatchType: "exact", Pattern: "flow-video-omni-flash", RequestTypes: []schemas.RequestType{schemas.VideoGenerationRequest}, PricingPatchJSON: `{"output_cost_per_video_per_second":0.05}`}}))
	pricing := modelcatalog.NewTestCatalogWithDatasheet(ds)
	for _, tc := range []struct {
		name    string
		model   string
		seconds int
		clips   int
		priced  bool
	}{
		{"native matching override", "flow-video-omni-flash", 8, 1, true},
		{"two returned clips", "flow-video-omni-flash", 8, 2, true},
		{"different native model", "flow-video-fast", 8, 1, false},
		{"missing duration", "flow-video-omni-flash", 0, 1, false},
		{"missing produced clip", "flow-video-omni-flash", 8, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := nativeVideoJob(t, schemas.Fastgen, "fastgen-rate-job", generationDims(tc.model, "16x9", tc.seconds))
			resp := &schemas.BifrostVideoGenerationResponse{Status: schemas.VideoStatusCompleted, Videos: make([]schemas.VideoOutput, tc.clips), ExtraFields: schemas.BifrostResponseExtraFields{ProviderResponseHeaders: map[string]string{"x-bf-provider-credits": "999"}}}
			settlement, err := NewVideoSettler(nil).Settle(context.Background(), pricing, JobRequest{Provider: schemas.Fastgen, ProviderJobID: job.JobID, Job: job, Scopes: &modelcatalog.PricingLookupScopes{Provider: provider}, Payload: VideoPayload{Response: resp}})
			require.NoError(t, err)
			assert.Equal(t, tc.priced, settlement.Priced)
			if tc.priced {
				assert.InDelta(t, float64(tc.seconds*tc.clips)*0.05, settlement.Cost, 1e-12)
			} else {
				assert.True(t, settlement.RecordUnpriced)
			}
		})
	}
}

func TestVideoSettle_FastgenFixedOverrideNeedsNoDuration(t *testing.T) {
	ds := datasheet.New(nil, bifrost.NewNoOpLogger(), datasheet.Config{})
	provider := string(schemas.Fastgen)
	require.NoError(t, ds.SetOverrides([]cstables.TablePricingOverride{{ID: "fastgen-flat", Name: "fixed USD rate", ScopeKind: "provider", ProviderID: &provider, MatchType: "exact", Pattern: "flow-video-fast", RequestTypes: []schemas.RequestType{schemas.VideoGenerationRequest}, PricingPatchJSON: `{"cost_per_request":0.75}`}}))
	pricing := modelcatalog.NewTestCatalogWithDatasheet(ds)
	job := nativeVideoJob(t, schemas.Fastgen, "fastgen-fixed", modelcatalog.VideoPricingDimensions{Model: "flow-video-fast", RequestType: schemas.VideoGenerationRequest})
	settlement, err := NewVideoSettler(nil).Settle(context.Background(), pricing, JobRequest{Provider: schemas.Fastgen, ProviderJobID: job.JobID, Job: job, Payload: VideoPayload{Response: &schemas.BifrostVideoGenerationResponse{Status: schemas.VideoStatusCompleted, Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}}}}})
	require.NoError(t, err)
	require.True(t, settlement.Priced)
	assert.InDelta(t, 0.75, settlement.Cost, 1e-12)
}

func TestVideoSweeper_FalWaitsForBillingThenAccountsOnce(t *testing.T) {
	for _, status := range []schemas.VideoStatus{schemas.VideoStatusCompleted, schemas.VideoStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			store := newFakeAccountingStore()
			job := nativeVideoJob(t, schemas.Fal, "fal-delayed-bill", generationDims("sora-2-pro", "1920x1080", 8))
			due := time.Now().Add(-time.Minute)
			job.NextCheckAt = &due
			require.NoError(t, store.UpsertProviderJob(context.Background(), job))
			retriever := &fakeVideoRetriever{resp: &schemas.BifrostVideoGenerationResponse{ID: job.JobID, Status: status}}
			reporter := &fakeUsageReporter{}
			sweeper := NewVideoSweeper(store, store, fakePricing{}, retriever, nil, reporter, SweeperConfig{})
			sweeper.SweepOnce(context.Background())
			assert.Equal(t, 1, retriever.calls)
			assert.Empty(t, store.logs, "billing pending cannot write an estimated cost")
			assert.Empty(t, reporter.reports)
			assert.NotEqual(t, cstables.ProviderJobAccountingStatusAccounted, store.jobs[job.ID].AccountingStatus)
			retriever.resp.Usage = &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0}}
			store.jobs[job.ID].NextCheckAt = &due
			sweeper.SweepOnce(context.Background())
			sweeper.SweepOnce(context.Background())
			assert.Equal(t, 2, retriever.calls, "an accounted job must no longer be retrieved")
			require.Len(t, store.logs, 1)
			require.Len(t, reporter.reports, 1)
			entry := store.logs[AccountingLogID(ProviderJobKindVideo, schemas.Fal, job.JobID)]
			require.NotNil(t, entry)
			require.NotNil(t, entry.Cost)
			assert.Zero(t, *entry.Cost)
			assert.Equal(t, "creating-key", entry.SelectedKeyID)
			assert.Equal(t, cstables.ProviderJobAccountingStatusAccounted, store.jobs[job.ID].AccountingStatus)
		})
	}
}

func TestAccountVideoJob_FalPendingDoesNotClaimAndActualCanRecover(t *testing.T) {
	store := newFakeAccountingStore()
	job := nativeVideoJob(t, schemas.Fal, "fal-inline", generationDims("sora-2-pro", "1920x1080", 8))
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))
	reporter := &fakeUsageReporter{}
	resp := &schemas.BifrostVideoGenerationResponse{ID: job.JobID, Status: schemas.VideoStatusCompleted}
	req := JobRequest{Provider: schemas.Fal, ProviderJobID: job.JobID, ClaimedBy: "inline", UsageReporter: reporter, Payload: VideoPayload{Response: resp}}
	out, err := AccountVideoJob(context.Background(), store, store, fakePricing{}, req)
	require.NoError(t, err)
	assert.Nil(t, out)
	assert.Equal(t, cstables.ProviderJobAccountingStatusPending, store.jobs[job.ID].AccountingStatus)
	assert.Empty(t, store.logs)
	// Exhausting the bounded background poll budget must not prevent recovery when
	// a later explicit retrieve finally carries the actual billed event.
	store.jobs[job.ID].AccountingStatus = cstables.ProviderJobAccountingStatusUnpriceable
	reason := UnpriceableReasonMaxPollAttempts
	store.jobs[job.ID].UnpriceableReason = &reason
	resp.Usage = &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0.42}}
	store.failCompleteOnce = true
	_, err = AccountVideoJob(context.Background(), store, store, fakePricing{}, req)
	require.Error(t, err, "simulate a crash after the aggregate and governance report")
	for i := 0; i < 2; i++ {
		out, err = AccountVideoJob(context.Background(), store, store, fakePricing{}, req)
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.InDelta(t, 0.42, out.Cost, 1e-12)
	}
	assert.Len(t, store.logs, 1)
	assert.Len(t, reporter.reports, 1, "retrying settlement must not report a second bill")
}

func TestVideoReprice_FalPreservesActualZeroAndRejectsEstimates(t *testing.T) {
	for _, cost := range []*float64{nil, bifrost.Ptr(0.0), bifrost.Ptr(0.42)} {
		entry := &logstore.Log{ID: "fal-reprice", Provider: string(schemas.Fal), Model: "sora-2-pro", VideoDebugParsed: &schemas.BifrostVideoDebug{Status: schemas.VideoStatusCompleted, Accounting: &schemas.VideoAccountingDebug{Seconds: bifrost.Ptr(8), Size: "1920x1080", OutputCount: 1, ProviderCost: cost}}}
		result, err := NewVideoSettler(nil).RepriceFromLog(entry, fakePricing{})
		if cost == nil {
			require.Error(t, err)
			assert.Nil(t, result)
		} else {
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, *cost, result.Cost)
		}
	}
}

func TestVideoReprice_FalActualZeroOnlyChangesWhenCostIgnored(t *testing.T) {
	ds := datasheet.New(nil, bifrost.NewNoOpLogger(), datasheet.Config{})
	provider := string(schemas.Fal)
	require.NoError(t, ds.SetOverrides([]cstables.TablePricingOverride{{ID: "fal-rate", Name: "configured USD rate", ScopeKind: "provider", ProviderID: &provider, MatchType: "exact", Pattern: "fal-ai/veo3.1", RequestTypes: []schemas.RequestType{schemas.VideoGenerationRequest}, PricingPatchJSON: `{"cost_per_request":0.75}`}}))
	pricing := modelcatalog.NewTestCatalogWithDatasheet(ds)
	entry := &logstore.Log{ID: "fal-reprice-zero", Provider: provider, Model: "fal-ai/veo3.1", VideoDebugParsed: &schemas.BifrostVideoDebug{Status: schemas.VideoStatusCompleted, Accounting: &schemas.VideoAccountingDebug{OutputCount: 1, ProviderCost: bifrost.Ptr(0.0)}}}
	actual, err := NewVideoSettler(nil).RepriceFromLog(entry, pricing)
	require.NoError(t, err)
	require.NotNil(t, actual)
	assert.Zero(t, actual.Cost)
	pricing.SetIgnoreProviderCost(schemas.Fal, true)
	ignored, err := NewVideoSettler(nil).RepriceFromLog(entry, pricing)
	require.NoError(t, err)
	require.NotNil(t, ignored)
	assert.InDelta(t, 0.75, ignored.Cost, 1e-12)
}

func BenchmarkVideoSettleNativeProviders(b *testing.B) {
	for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.Fal, schemas.Fastgen} {
		b.Run(string(provider), func(b *testing.B) {
			params, err := MarshalVideoDimensions(generationDims("sora-2-pro", "1920x1080", 8))
			if err != nil {
				b.Fatal(err)
			}
			job := &cstables.TableProviderJob{JobID: "benchmark-video", Params: params}
			resp := &schemas.BifrostVideoGenerationResponse{ID: job.JobID, Status: schemas.VideoStatusCompleted, Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}}, Usage: &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0.42}}}
			req := JobRequest{Provider: provider, ProviderJobID: job.JobID, Job: job, Payload: VideoPayload{Response: resp}}
			settler := NewVideoSettler(nil)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := settler.Settle(context.Background(), fakePricing{}, req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkVideoSettleNativeCatalogRates(b *testing.B) {
	for _, tc := range []struct{ name, patch string }{
		{"flat", `{"cost_per_request":0.75}`},
		{"per-second", `{"output_cost_per_video_per_second":0.05}`},
		{"mixed", `{"cost_per_request":0.75,"output_cost_per_video_per_second":0.05}`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ds := datasheet.New(nil, bifrost.NewNoOpLogger(), datasheet.Config{})
			provider := string(schemas.Fastgen)
			if err := ds.SetOverrides([]cstables.TablePricingOverride{{ID: "bench-native", Name: "native", ScopeKind: "provider", ProviderID: &provider, MatchType: "exact", Pattern: "flow-video-omni-flash", RequestTypes: []schemas.RequestType{schemas.VideoGenerationRequest}, PricingPatchJSON: tc.patch}}); err != nil {
				b.Fatal(err)
			}
			pricing := modelcatalog.NewTestCatalogWithDatasheet(ds)
			params, err := MarshalVideoDimensions(generationDims("flow-video-omni-flash", "16x9", 8))
			if err != nil {
				b.Fatal(err)
			}
			job := &cstables.TableProviderJob{JobID: "bench-native", Params: params}
			resp := &schemas.BifrostVideoGenerationResponse{Status: schemas.VideoStatusCompleted, Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}}}
			req := JobRequest{Provider: schemas.Fastgen, ProviderJobID: job.JobID, Job: job, Payload: VideoPayload{Response: resp}}
			settler := NewVideoSettler(nil)
			if result, err := settler.Settle(context.Background(), pricing, req); err != nil || !result.Priced {
				b.Fatalf("native rate must price: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := settler.Settle(context.Background(), pricing, req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
