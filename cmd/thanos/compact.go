// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/oklog/run"
	"github.com/oklog/ulid"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/route"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/indexheader"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/compact"
	"github.com/thanos-io/thanos/pkg/compact/downsample"
	"github.com/thanos-io/thanos/pkg/component"
	"github.com/thanos-io/thanos/pkg/extflag"
	"github.com/thanos-io/thanos/pkg/extprom"
	extpromhttp "github.com/thanos-io/thanos/pkg/extprom/http"
	"github.com/thanos-io/thanos/pkg/objstore"
	"github.com/thanos-io/thanos/pkg/objstore/client"
	"github.com/thanos-io/thanos/pkg/prober"
	"github.com/thanos-io/thanos/pkg/runutil"
	httpserver "github.com/thanos-io/thanos/pkg/server/http"
	"github.com/thanos-io/thanos/pkg/ui"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	metricIndexGenerateName = "thanos_compact_generated_index_total"
	metricIndexGenerateHelp = "Total number of generated indexes."
)

var (
	compactions = compactionSet{
		1 * time.Hour,
		2 * time.Hour,
		8 * time.Hour,
		2 * 24 * time.Hour,
		14 * 24 * time.Hour,
	}
)

type compactionSet []time.Duration

func (cs compactionSet) String() string {
	result := make([]string, len(cs))
	for i, c := range cs {
		result[i] = fmt.Sprintf("%d=%dh", i, int(c.Hours()))
	}
	return strings.Join(result, ", ")
}

// levels returns set of compaction levels not higher than specified max compaction level.
func (cs compactionSet) levels(maxLevel int) ([]int64, error) {
	if maxLevel >= len(cs) {
		return nil, errors.Errorf("level is bigger then default set of %d", len(cs))
	}

	levels := make([]int64, maxLevel+1)
	for i, c := range cs[:maxLevel+1] {
		levels[i] = int64(c / time.Millisecond)
	}
	return levels, nil
}

// maxLevel returns max available compaction level.
func (cs compactionSet) maxLevel() int {
	return len(cs) - 1
}

func registerCompact(m map[string]setupFunc, app *kingpin.Application) {
	cmd := app.Command(component.Compact.String(), "continuously compacts blocks in an object store bucket")
	conf := &compactConfig{}
	conf.registerFlag(cmd)

	m[component.Compact.String()] = func(g *run.Group, logger log.Logger, reg *prometheus.Registry, tracer opentracing.Tracer, _ <-chan struct{}, _ bool) error {
		return runCompact(g, logger, reg, component.Compact, *conf)
	}
}

func runCompact(
	g *run.Group,
	logger log.Logger,
	reg *prometheus.Registry,
	component component.Component,
	conf compactConfig,
) error {
	deleteDelay := time.Duration(conf.deleteDelay)
	halted := promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Name: "thanos_compactor_halted",
		Help: "Set to 1 if the compactor halted due to an unexpected error.",
	})
	halted.Set(0)
	retried := promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "thanos_compactor_retries_total",
		Help: "Total number of retries after retriable compactor error.",
	})
	iterations := promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "thanos_compactor_iterations_total",
		Help: "Total number of iterations that were executed successfully.",
	})
	partialUploadDeleteAttempts := promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "thanos_compactor_aborted_partial_uploads_deletion_attempts_total",
		Help: "Total number of started deletions of blocks that are assumed aborted and only partially uploaded.",
	})
	blocksCleaned := promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "thanos_compactor_blocks_cleaned_total",
		Help: "Total number of blocks deleted in compactor.",
	})
	blockCleanupFailures := promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "thanos_compactor_block_cleanup_failures_total",
		Help: "Failures encountered while deleting blocks in compactor.",
	})
	blocksMarkedForDeletion := promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: "thanos_compactor_blocks_marked_for_deletion_total",
		Help: "Total number of blocks marked for deletion in compactor.",
	})
	_ = promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{
		Name: "thanos_delete_delay_seconds",
		Help: "Configured delete delay in seconds.",
	}, func() float64 {
		return deleteDelay.Seconds()
	})

	downsampleMetrics := newDownsampleMetrics(reg)

	httpProbe := prober.NewHTTP()
	statusProber := prober.Combine(
		httpProbe,
		prober.NewInstrumentation(component, logger, extprom.WrapRegistererWithPrefix("thanos_", reg)),
	)

	srv := httpserver.New(logger, reg, component, httpProbe,
		httpserver.WithListen(conf.http.bindAddress),
		httpserver.WithGracePeriod(time.Duration(conf.http.gracePeriod)),
	)

	g.Add(func() error {
		statusProber.Healthy()

		return srv.ListenAndServe()
	}, func(err error) {
		statusProber.NotReady(err)
		defer statusProber.NotHealthy(err)

		srv.Shutdown(err)
	})

	confContentYaml, err := conf.objStore.Content()
	if err != nil {
		return err
	}

	bkt, err := client.NewBucket(logger, confContentYaml, reg, component.String())
	if err != nil {
		return err
	}

	relabelContentYaml, err := conf.selectorRelabelConf.Content()
	if err != nil {
		return errors.Wrap(err, "get content of relabel configuration")
	}

	relabelConfig, err := parseRelabelConfig(relabelContentYaml)
	if err != nil {
		return err
	}

	// Ensure we close up everything properly.
	defer func() {
		if err != nil {
			runutil.CloseWithLogOnErr(logger, bkt, "bucket client")
		}
	}()

	// While fetching blocks, we filter out blocks that were marked for deletion by using IgnoreDeletionMarkFilter.
	// The delay of deleteDelay/2 is added to ensure we fetch blocks that are meant to be deleted but do not have a replacement yet.
	// This is to make sure compactor will not accidentally perform compactions with gap instead.
	ignoreDeletionMarkFilter := block.NewIgnoreDeletionMarkFilter(logger, bkt, deleteDelay/2)
	duplicateBlocksFilter := block.NewDeduplicateFilter()

	baseMetaFetcher, err := block.NewBaseFetcher(logger, 32, bkt, "", extprom.WrapRegistererWithPrefix("thanos_", reg))
	if err != nil {
		return errors.Wrap(err, "create meta fetcher")
	}

	enableVerticalCompaction := false
	if len(conf.dedupReplicaLabels) > 0 {
		enableVerticalCompaction = true
		level.Info(logger).Log(
			"msg", "deduplication.replica-label specified, vertical compaction is enabled",
			"dedupReplicaLabels",
			strings.Join(conf.dedupReplicaLabels, ","),
		)
	}

	compactorView := ui.NewBucketUI(
		logger,
		conf.label,
		path.Join(conf.webConf.externalPrefix, "/loaded"),
		conf.webConf.prefixHeaderName,
	)
	var sy *compact.Syncer
	{
		// Make sure all compactor meta syncs are done through Syncer.SyncMeta for readability.
		cf := baseMetaFetcher.NewMetaFetcher(
			extprom.WrapRegistererWithPrefix("thanos_", reg), []block.MetadataFilter{
				block.NewLabelShardedMetaFilter(relabelConfig),
				block.NewConsistencyDelayMetaFilter(logger, conf.consistencyDelay, extprom.WrapRegistererWithPrefix("thanos_", reg)),
				ignoreDeletionMarkFilter,
				duplicateBlocksFilter,
			}, []block.MetadataModifier{block.NewReplicaLabelRemover(logger, conf.dedupReplicaLabels)},
		)
		cf.UpdateOnChange(compactorView.Set)
		sy, err = compact.NewSyncer(
			logger,
			reg,
			bkt,
			cf,
			duplicateBlocksFilter,
			ignoreDeletionMarkFilter,
			blocksMarkedForDeletion,
			conf.blockSyncConcurrency,
			conf.acceptMalformedIndex, enableVerticalCompaction)
		if err != nil {
			return errors.Wrap(err, "create syncer")
		}
	}

	levels, err := compactions.levels(conf.maxCompactionLevel)
	if err != nil {
		return errors.Wrap(err, "get compaction levels")
	}

	if conf.maxCompactionLevel < compactions.maxLevel() {
		level.Warn(logger).Log("msg", "Max compaction level is lower than should be", "current", conf.maxCompactionLevel, "default", compactions.maxLevel())
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Instantiate the compactor with different time slices. Timestamps in TSDB
	// are in milliseconds.
	comp, err := tsdb.NewLeveledCompactor(ctx, reg, logger, levels, downsample.NewPool())
	if err != nil {
		cancel()
		return errors.Wrap(err, "create compactor")
	}

	var (
		compactDir      = path.Join(conf.dataDir, "compact")
		downsamplingDir = path.Join(conf.dataDir, "downsample")
		indexCacheDir   = path.Join(conf.dataDir, "index_cache")
	)

	if err := os.RemoveAll(downsamplingDir); err != nil {
		cancel()
		return errors.Wrap(err, "clean working downsample directory")
	}

	blocksCleaner := compact.NewBlocksCleaner(logger, bkt, ignoreDeletionMarkFilter, deleteDelay, blocksCleaned, blockCleanupFailures)
	compactor, err := compact.NewBucketCompactor(logger, sy, comp, compactDir, bkt, conf.compactionConcurrency)
	if err != nil {
		cancel()
		return errors.Wrap(err, "create bucket compactor")
	}

	retentionByResolution := map[compact.ResolutionLevel]time.Duration{
		compact.ResolutionLevelRaw: time.Duration(conf.retentionRaw),
		compact.ResolutionLevel5m:  time.Duration(conf.retentionFiveMin),
		compact.ResolutionLevel1h:  time.Duration(conf.retentionOneHr),
	}

	if retentionByResolution[compact.ResolutionLevelRaw].Seconds() != 0 {
		level.Info(logger).Log("msg", "retention policy of raw samples is enabled", "duration", retentionByResolution[compact.ResolutionLevelRaw])
	}
	if retentionByResolution[compact.ResolutionLevel5m].Seconds() != 0 {
		level.Info(logger).Log("msg", "retention policy of 5 min aggregated samples is enabled", "duration", retentionByResolution[compact.ResolutionLevel5m])
	}
	if retentionByResolution[compact.ResolutionLevel1h].Seconds() != 0 {
		level.Info(logger).Log("msg", "retention policy of 1 hour aggregated samples is enabled", "duration", retentionByResolution[compact.ResolutionLevel1h])
	}

	compactMainFn := func() error {
		if err := compactor.Compact(ctx); err != nil {
			return errors.Wrap(err, "compaction")
		}

		if !conf.disableDownsampling {
			// After all compactions are done, work down the downsampling backlog.
			// We run two passes of this to ensure that the 1h downsampling is generated
			// for 5m downsamplings created in the first run.
			level.Info(logger).Log("msg", "start first pass of downsampling")
			if err := sy.SyncMetas(ctx); err != nil {
				return errors.Wrap(err, "sync before first pass of downsampling")
			}
			if err := downsampleBucket(ctx, logger, downsampleMetrics, bkt, sy.Metas(), downsamplingDir); err != nil {
				return errors.Wrap(err, "first pass of downsampling failed")
			}

			level.Info(logger).Log("msg", "start second pass of downsampling")
			if err := sy.SyncMetas(ctx); err != nil {
				return errors.Wrap(err, "sync before second pass of downsampling")
			}
			if err := downsampleBucket(ctx, logger, downsampleMetrics, bkt, sy.Metas(), downsamplingDir); err != nil {
				return errors.Wrap(err, "second pass of downsampling failed")
			}
			level.Info(logger).Log("msg", "downsampling iterations done")
		} else {
			level.Info(logger).Log("msg", "downsampling was explicitly disabled")
		}

		// TODO(bwplotka): Find a way to avoid syncing if no op was done.
		if err := sy.SyncMetas(ctx); err != nil {
			return errors.Wrap(err, "sync before first pass of downsampling")
		}

		if err := compact.ApplyRetentionPolicyByResolution(ctx, logger, bkt, sy.Metas(), retentionByResolution, blocksMarkedForDeletion); err != nil {
			return errors.Wrap(err, "retention failed")
		}

		// No need to resync before partial uploads and delete marked blocks. Last sync should be valid.
		compact.BestEffortCleanAbortedPartialUploads(ctx, logger, sy.Partial(), bkt, partialUploadDeleteAttempts, blocksCleaned, blockCleanupFailures)
		if err := blocksCleaner.DeleteMarkedBlocks(ctx); err != nil {
			return errors.Wrap(err, "error cleaning blocks")
		}
		return nil
	}

	g.Add(func() error {
		defer runutil.CloseWithLogOnErr(logger, bkt, "bucket client")

		// Generate index files.
		// TODO(bwplotka): Remove this in next release.
		if conf.generateMissingIndexCacheFiles {
			if err := sy.SyncMetas(ctx); err != nil {
				return err
			}
			if err := genMissingIndexCacheFiles(ctx, logger, reg, bkt, sy.Metas(), indexCacheDir); err != nil {
				return err
			}
		}

		if !conf.wait {
			return compactMainFn()
		}

		// --wait=true is specified.
		return runutil.Repeat(conf.waitInterval, ctx.Done(), func() error {
			err := compactMainFn()
			if err == nil {
				iterations.Inc()
				return nil
			}

			// The HaltError type signals that we hit a critical bug and should block
			// for investigation. You should alert on this being halted.
			if compact.IsHaltError(err) {
				if conf.haltOnError {
					level.Error(logger).Log("msg", "critical error detected; halting", "err", err)
					halted.Set(1)
					select {}
				} else {
					return errors.Wrap(err, "critical error detected")
				}
			}

			// The RetryError signals that we hit an retriable error (transient error, no connection).
			// You should alert on this being triggered too frequently.
			if compact.IsRetryError(err) {
				level.Error(logger).Log("msg", "retriable error", "err", err)
				retried.Inc()
				// TODO(bplotka): use actual "retry()" here instead of waiting 5 minutes?
				return nil
			}

			return errors.Wrap(err, "error executing compaction")
		})
	}, func(error) {
		cancel()
	})

	if conf.wait {
		r := route.New()

		ins := extpromhttp.NewInstrumentationMiddleware(reg)
		compactorView.Register(r, ins)

		global := ui.NewBucketUI(logger, conf.label, path.Join(conf.webConf.externalPrefix, "/global"), conf.webConf.prefixHeaderName)
		global.Register(r, ins)

		// Separate fetcher for global view.
		// TODO(bwplotka): Allow Bucket UI to visualize the state of the block as well.
		f := baseMetaFetcher.NewMetaFetcher(extprom.WrapRegistererWithPrefix("thanos_bucket_ui", reg), nil, nil, "component", "globalBucketUI")
		f.UpdateOnChange(global.Set)

		srv.Handle("/", r)

		g.Add(func() error {
			iterCtx, iterCancel := context.WithTimeout(ctx, conf.waitInterval)
			_, _, _ = f.Fetch(iterCtx)
			iterCancel()

			// For /global state make sure to fetch periodically.
			return runutil.Repeat(time.Minute, ctx.Done(), func() error {
				return runutil.RetryWithLog(logger, time.Minute, ctx.Done(), func() error {
					iterCtx, iterCancel := context.WithTimeout(ctx, conf.waitInterval)
					defer iterCancel()

					_, _, err := f.Fetch(iterCtx)
					if err != nil {
						return err
					}
					return nil
				})
			})
		}, func(error) {
			cancel()
		})
	}

	level.Info(logger).Log("msg", "starting compact node")
	statusProber.Ready()
	return nil
}

// genMissingIndexCacheFiles scans over all blocks, generates missing index cache files and uploads them to object storage.
func genMissingIndexCacheFiles(ctx context.Context, logger log.Logger, reg *prometheus.Registry, bkt objstore.Bucket, metas map[ulid.ULID]*metadata.Meta, dir string) error {
	genIndex := promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name: metricIndexGenerateName,
		Help: metricIndexGenerateHelp,
	})

	if err := os.RemoveAll(dir); err != nil {
		return errors.Wrap(err, "clean index cache directory")
	}
	if err := os.MkdirAll(dir, 0777); err != nil {
		return errors.Wrap(err, "create dir")
	}

	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			level.Error(logger).Log("msg", "failed to remove index cache directory", "path", dir, "err", err)
		}
	}()

	level.Info(logger).Log("msg", "start index cache processing")

	for _, meta := range metas {
		// New version of compactor pushes index cache along with data block.
		// Skip uncompacted blocks.
		if meta.Compaction.Level == 1 {
			continue
		}

		if err := generateIndexCacheFile(ctx, bkt, logger, dir, meta); err != nil {
			return err
		}
		genIndex.Inc()
	}

	level.Info(logger).Log("msg", "generating index cache files is done, you can remove startup argument `index.generate-missing-cache-file`")
	return nil
}

func generateIndexCacheFile(
	ctx context.Context,
	bkt objstore.Bucket,
	logger log.Logger,
	indexCacheDir string,
	meta *metadata.Meta,
) error {
	id := meta.ULID

	bdir := filepath.Join(indexCacheDir, id.String())
	if err := os.MkdirAll(bdir, 0777); err != nil {
		return errors.Wrap(err, "create block dir")
	}

	defer func() {
		if err := os.RemoveAll(bdir); err != nil {
			level.Error(logger).Log("msg", "failed to remove index cache directory", "path", bdir, "err", err)
		}
	}()

	cachePath := filepath.Join(bdir, block.IndexCacheFilename)
	cache := path.Join(meta.ULID.String(), block.IndexCacheFilename)

	ok, err := bkt.Exists(ctx, cache)
	if ok {
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "attempt to check if a cached index file exists")
	}

	level.Debug(logger).Log("msg", "make index cache", "block", id)

	// Try to download index file from obj store.
	indexPath := filepath.Join(bdir, block.IndexFilename)
	index := path.Join(id.String(), block.IndexFilename)

	if err := objstore.DownloadFile(ctx, logger, bkt, index, indexPath); err != nil {
		return errors.Wrap(err, "download index file")
	}

	if err := indexheader.WriteJSON(logger, indexPath, cachePath); err != nil {
		return errors.Wrap(err, "write index cache")
	}

	if err := objstore.UploadFile(ctx, logger, bkt, cachePath, cache); err != nil {
		return errors.Wrap(err, "upload index cache")
	}
	return nil
}

type compactConfig struct {
	haltOnError                                    bool
	acceptMalformedIndex                           bool
	maxCompactionLevel                             int
	http                                           httpConfig
	dataDir                                        string
	objStore                                       extflag.PathOrContent
	consistencyDelay                               time.Duration
	retentionRaw, retentionFiveMin, retentionOneHr model.Duration
	wait                                           bool
	waitInterval                                   time.Duration
	generateMissingIndexCacheFiles                 bool
	disableDownsampling                            bool
	blockSyncConcurrency                           int
	compactionConcurrency                          int
	deleteDelay                                    model.Duration
	dedupReplicaLabels                             []string
	selectorRelabelConf                            extflag.PathOrContent
	webConf                                        webConfig
	label                                          string
}

func (cc *compactConfig) registerFlag(cmd *kingpin.CmdClause) *compactConfig {
	cmd.Flag("debug.halt-on-error", "Halt the process if a critical compaction error is detected.").
		Hidden().Default("true").BoolVar(&cc.haltOnError)
	cmd.Flag("debug.accept-malformed-index",
		"Compaction index verification will ignore out of order label names.").
		Hidden().Default("false").BoolVar(&cc.acceptMalformedIndex)
	cmd.Flag("debug.max-compaction-level", fmt.Sprintf("Maximum compaction level, default is %d: %s", compactions.maxLevel(), compactions.String())).
		Hidden().Default(strconv.Itoa(compactions.maxLevel())).IntVar(&cc.maxCompactionLevel)

	cc.http.registerFlag(cmd)

	cmd.Flag("data-dir", "Data directory in which to cache blocks and process compactions.").
		Default("./data").StringVar(&cc.dataDir)

	cc.objStore = *regCommonObjStoreFlags(cmd, "", false)

	cmd.Flag("consistency-delay", fmt.Sprintf("Minimum age of fresh (non-compacted) blocks before they are being processed. Malformed blocks older than the maximum of consistency-delay and %v will be removed.", compact.PartialUploadThresholdAge)).
		Default("30m").DurationVar(&cc.consistencyDelay)

	cmd.Flag("retention.resolution-raw",
		"How long to retain raw samples in bucket. Setting this to 0d will retain samples of this resolution forever").
		Default("0d").SetValue(&cc.retentionRaw)
	cmd.Flag("retention.resolution-5m", "How long to retain samples of resolution 1 (5 minutes) in bucket. Setting this to 0d will retain samples of this resolution forever").
		Default("0d").SetValue(&cc.retentionFiveMin)
	cmd.Flag("retention.resolution-1h", "How long to retain samples of resolution 2 (1 hour) in bucket. Setting this to 0d will retain samples of this resolution forever").
		Default("0d").SetValue(&cc.retentionOneHr)

	// TODO(kakkoyun, pgough): https://github.com/thanos-io/thanos/issues/2266.
	cmd.Flag("wait", "Do not exit after all compactions have been processed and wait for new work.").
		Short('w').BoolVar(&cc.wait)
	cmd.Flag("wait-interval", "Wait interval between consecutive compaction runs and bucket refreshes. Only works when --wait flag specified.").
		Default("5m").DurationVar(&cc.waitInterval)

	cmd.Flag("index.generate-missing-cache-file", "DEPRECATED flag. Will be removed in next release. If enabled, on startup compactor runs an on-off job that scans all the blocks to find all blocks with missing index cache file. It generates those if needed and upload.").
		Hidden().Default("false").BoolVar(&cc.generateMissingIndexCacheFiles)

	cmd.Flag("downsampling.disable", "Disables downsampling. This is not recommended "+
		"as querying long time ranges without non-downsampled data is not efficient and useful e.g it is not possible to render all samples for a human eye anyway").
		Default("false").BoolVar(&cc.disableDownsampling)

	cmd.Flag("block-sync-concurrency", "Number of goroutines to use when syncing block metadata from object storage.").
		Default("20").IntVar(&cc.blockSyncConcurrency)

	cmd.Flag("compact.concurrency", "Number of goroutines to use when compacting groups.").
		Default("1").IntVar(&cc.compactionConcurrency)

	cmd.Flag("delete-delay", "Time before a block marked for deletion is deleted from bucket. "+
		"If delete-delay is non zero, blocks will be marked for deletion and compactor component will delete blocks marked for deletion from the bucket. "+
		"If delete-delay is 0, blocks will be deleted straight away. "+
		"Note that deleting blocks immediately can cause query failures, if store gateway still has the block loaded, "+
		"or compactor is ignoring the deletion because it's compacting the block at the same time.").
		Default("48h").SetValue(&cc.deleteDelay)

	cmd.Flag("deduplication.replica-label", "Label to treat as a replica indicator of blocks that can be deduplicated (repeated flag). This will merge multiple replica blocks into one. This process is irreversible."+
		"Experimental. When it is set to true, compactor will ignore the given labels so that vertical compaction can merge the blocks."+
		"Please note that this uses a NAIVE algorithm for merging (no smart replica deduplication, just chaining samples together)."+
		"This works well for deduplication of blocks with **precisely the same samples** like produced by Receiver replication.").
		Hidden().StringsVar(&cc.dedupReplicaLabels)

	cc.selectorRelabelConf = *regSelectorRelabelFlags(cmd)

	cc.webConf.registerFlag(cmd)

	cmd.Flag("bucket-web-label", "Prometheus label to use as timeline title in the bucket web UI").StringVar(&cc.label)

	return cc
}
