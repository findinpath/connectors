package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	boilerplate "github.com/estuary/connectors/source-boilerplate"
	pc "github.com/estuary/flow/go/protocols/capture"
	pf "github.com/estuary/flow/go/protocols/flow"
	"golang.org/x/sync/errgroup"
)

const (
	// Limit on how recent a change event can be per its ApproximateCreationDateTime to allow it to
	// be captured while streaming during a backfill.
	backfillStreamingHorizon = 1 * time.Hour

	// Default number of items to evaluate for each backfill scan request for tables with relatively
	// low provisioned read capacity units.
	lowRcuScanLimit = 100

	// Delay between checks of the shard topology for the streaming process.
	defaultShardMonitorDelay = 5 * time.Second

	// Number of backfill segments that will be used per table.
	defaultBackfillSegments = 100

	// Time to spend for each period of backfilling a table.
	backfillDuration = 1 * time.Hour

	// Number of concurrent workers to use when backfilling a table.
	backfillConcurrency = 3

	// Number of stream workers which may concurrently send requests for stream data.
	streamConcurrency = 5
)

type table struct {
	tableName      string
	stateKey       boilerplate.StateKey
	bindingIdx     int
	keyFields      []string // Ordered as partition key followed by sort key (if there is a sort key)
	activeSegments chan int

	effectiveRcus       int
	scanLimitFromConfig int
	shardMonitorDelay   time.Duration

	totalBackfillSegments int
	streamArn             string
	backfillComplete      bool
}

func (driver) Pull(open *pc.Request_Open, stream *boilerplate.PullOutput) error {
	ctx := stream.Context()

	var cfg config
	if err := pf.UnmarshalStrict(open.Capture.ConfigJson, &cfg); err != nil {
		return fmt.Errorf("parsing endpoint config: %w", err)
	}

	client, err := cfg.toClient(ctx)
	if err != nil {
		return fmt.Errorf("creating client: %w", err)
	}

	var checkpoint captureState
	if open.StateJson != nil && !reflect.DeepEqual(open.StateJson, json.RawMessage("{}")) {
		if err := json.Unmarshal(open.StateJson, &checkpoint); err != nil {
			return fmt.Errorf("parsing driver checkpoint: %w", err)
		}
	} else {
		// Capture has never emitted a checkpoint.
		checkpoint.Tables = make(map[boilerplate.StateKey]tableState)
	}

	migrated, err := migrateState(&checkpoint, open.Capture.Bindings)
	if err != nil {
		return err
	}

	c := capture{
		client: client,
		stream: stream,
		config: cfg,
		state:  checkpoint,
	}

	tables := []*table{}
	for idx, binding := range open.Capture.Bindings {
		var res resource
		if err := pf.UnmarshalStrict(binding.ResourceConfigJson, &res); err != nil {
			return fmt.Errorf("parsing resource config: %w", err)
		}

		t, err := c.initializeTable(ctx, idx, res.Table, boilerplate.StateKey(binding.StateKey), res.RcuAllocation)
		if err != nil {
			return fmt.Errorf("initializing table: %w", err)
		}

		tables = append(tables, t)
	}

	if err := stream.Ready(false); err != nil {
		return err
	}

	eg, groupCtx := errgroup.WithContext(ctx)

	if migrated {
		if err := c.checkpoint(); err != nil {
			return fmt.Errorf("updating migrated checkpoint: %w", err)
		}
	}

	for _, table := range tables {
		table := table
		eg.Go(func() error {
			return c.captureTable(groupCtx, table)
		})
	}

	return eg.Wait()
}
