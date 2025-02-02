package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	rhpv2 "go.sia.tech/core/rhp/v2"
	"go.sia.tech/core/types"
	"go.sia.tech/renterd/api"
	"go.sia.tech/renterd/object"
	"go.sia.tech/renterd/tracing"
	"go.uber.org/zap"
	"lukechampine.com/frand"
)

const (
	downloadOverheadB             = 284
	maxConcurrentSectorsPerHost   = 3
	maxConcurrentSlabsPerDownload = 3
)

type (
	// id is a unique identifier used for debugging
	id [8]byte

	downloadManager struct {
		hp     hostProvider
		logger *zap.SugaredLogger

		maxOverdrive     uint64
		overdriveTimeout time.Duration

		statsOverdrivePct                *dataPoints
		statsSlabDownloadSpeedBytesPerMS *dataPoints

		stopChan chan struct{}

		mu            sync.Mutex
		ongoing       map[slabID]struct{}
		downloaders   map[types.PublicKey]*downloader
		lastRecompute time.Time
	}

	downloader struct {
		host hostV3

		statsDownloadSpeedBytesPerMS    *dataPoints // keep track of this separately for stats (no decay is applied)
		statsSectorDownloadEstimateInMS *dataPoints

		signalWorkChan chan struct{}
		stopChan       chan struct{}

		mu                  sync.Mutex
		consecutiveFailures uint64
		queue               []*sectorDownloadReq
		numDownloads        uint64
	}

	downloaderStats struct {
		avgSpeedMBPS float64
		healthy      bool
		numDownloads uint64
	}

	slabDownload struct {
		mgr *downloadManager

		dID       id
		sID       slabID
		created   time.Time
		index     int
		minShards int
		length    uint32
		offset    uint32

		mu             sync.Mutex
		lastOverdrive  time.Time
		numCompleted   int
		numInflight    uint64
		numLaunched    uint64
		numOverdriving uint64

		curr          types.PublicKey
		hostToSectors map[types.PublicKey][]sectorInfo
		used          map[types.PublicKey]struct{}

		sectors [][]byte
		errs    HostErrorSet
	}

	slabDownloadResponse struct {
		shards [][]byte
		index  int
		err    error
	}

	sectorDownloadReq struct {
		ctx context.Context

		length uint32
		offset uint32
		root   types.Hash256
		hk     types.PublicKey

		overdrive    bool
		sectorIndex  int
		responseChan chan sectorDownloadResp
	}

	sectorDownloadResp struct {
		overdrive   bool
		hk          types.PublicKey
		sectorIndex int
		sector      []byte
		err         error
	}

	sectorInfo struct {
		object.Sector
		index int
	}

	downloadManagerStats struct {
		avgDownloadSpeedMBPS float64
		avgOverdrivePct      float64
		downloaders          map[types.PublicKey]downloaderStats
	}
)

func (w *worker) initDownloadManager(maxOverdrive uint64, overdriveTimeout time.Duration, logger *zap.SugaredLogger) {
	if w.downloadManager != nil {
		panic("download manager already initialized") // developer error
	}

	w.downloadManager = newDownloadManager(w, maxOverdrive, overdriveTimeout, logger)
}

func newDownloadManager(hp hostProvider, maxOverdrive uint64, overdriveTimeout time.Duration, logger *zap.SugaredLogger) *downloadManager {
	return &downloadManager{
		hp:     hp,
		logger: logger,

		maxOverdrive:     maxOverdrive,
		overdriveTimeout: overdriveTimeout,

		statsOverdrivePct:                newDataPoints(0),
		statsSlabDownloadSpeedBytesPerMS: newDataPoints(0),

		stopChan: make(chan struct{}),

		ongoing:     make(map[slabID]struct{}),
		downloaders: make(map[types.PublicKey]*downloader),
	}
}

func newDownloader(host hostV3) *downloader {
	return &downloader{
		host: host,

		statsSectorDownloadEstimateInMS: newDataPoints(statsDecayHalfTime),
		statsDownloadSpeedBytesPerMS:    newDataPoints(0), // no decay for exposed stats

		signalWorkChan: make(chan struct{}, 1),
		stopChan:       make(chan struct{}),

		queue: make([]*sectorDownloadReq, 0),
	}
}

func (mgr *downloadManager) DownloadObject(ctx context.Context, w io.Writer, o object.Object, offset, length uint64, contracts []api.ContractMetadata) (err error) {
	// add tracing
	ctx, span := tracing.Tracer.Start(ctx, "download")
	defer func() {
		span.RecordError(err)
		span.End()
	}()

	// create identifier
	id := newID()

	// calculate what slabs we need
	slabs := slabsForDownload(o.Slabs, offset, length)
	if len(slabs) == 0 {
		return nil
	}

	// ensure everything cancels if download is done
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// refresh the downloaders
	mgr.refreshDownloaders(contracts)

	// build a map to count available shards later
	hosts := make(map[types.PublicKey]struct{})
	for _, c := range contracts {
		hosts[c.HostKey] = struct{}{}
	}

	// create the cipher writer
	cw := o.Key.Decrypt(w, offset)

	// create the trigger chan
	nextSlabChan := make(chan struct{}, 1)
	nextSlabChan <- struct{}{}

	// launch a goroutine to launch consecutive slab downloads
	responseChan := make(chan *slabDownloadResponse)
	defer close(responseChan)
	go func() {
		var slabIndex int

		for {
			if slabIndex < len(slabs) {
				next := slabs[slabIndex]

				// check if we have enough downloaders
				var available uint8
				for _, s := range next.Shards {
					if _, exists := hosts[s.Host]; exists {
						available++
					}
				}
				if available < next.MinShards {
					responseChan <- &slabDownloadResponse{err: fmt.Errorf("not enough hosts available to download the slab: %v/%v", available, next.MinShards)}
					return
				}

				// launch the download
				go mgr.downloadSlab(ctx, id, next, slabIndex, responseChan, nextSlabChan)
				slabIndex++
			}

			// wait for the trigger to launch the next one
			select {
			case <-ctx.Done():
				return
			case <-nextSlabChan:
			}
		}
	}()

	// collect the response, responses might come in out of order so we keep
	// them in a map and return what we can when we can
	responses := make(map[int]*slabDownloadResponse)
	var respIndex int
outer:
	for {
		select {
		case <-mgr.stopChan:
			return errors.New("manager was stopped")
		case <-ctx.Done():
			return errors.New("download timed out")
		case resp := <-responseChan:
			if resp.err != nil {
				mgr.logger.Errorf("download slab %v failed: %v", resp.index, resp.err)
				return resp.err
			}

			responses[resp.index] = resp
			for {
				if next, exists := responses[respIndex]; exists {
					slabs[respIndex].Decrypt(next.shards)
					err := slabs[respIndex].Recover(cw, next.shards)
					if err != nil {
						mgr.logger.Errorf("failed to recover slab %v: %v", respIndex, err)
						return err
					}
					next = nil
					delete(responses, respIndex)
					respIndex++
					continue
				} else {
					break
				}
			}

			// exit condition
			if respIndex == len(slabs) {
				break outer
			}
		}
	}

	return nil
}

func (mgr *downloadManager) DownloadSlab(ctx context.Context, slab object.Slab, contracts []api.ContractMetadata) ([][]byte, error) {
	// refresh the downloaders
	mgr.refreshDownloaders(contracts)

	// grab available hosts
	available := make(map[types.PublicKey]struct{})
	for _, c := range contracts {
		available[c.HostKey] = struct{}{}
	}

	// count how many shards we can download (best-case)
	var availableShards uint8
	for _, shard := range slab.Shards {
		if _, exists := available[shard.Host]; exists {
			availableShards++
		}
	}

	// check if we have enough shards
	if availableShards < slab.MinShards {
		return nil, fmt.Errorf("not enough hosts available to download the slab: %v/%v", availableShards, slab.MinShards)
	}

	// create identifier
	id := newID()

	// download the slab
	responseChan := make(chan *slabDownloadResponse)
	nextSlabChan := make(chan struct{})
	slice := object.SlabSlice{
		Slab:   slab,
		Offset: 0,
		Length: uint32(slab.MinShards) * rhpv2.SectorSize,
	}
	go mgr.downloadSlab(ctx, id, slice, 0, responseChan, nextSlabChan)

	// await the response
	var resp *slabDownloadResponse
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp = <-responseChan:
		if resp.err != nil {
			return nil, resp.err
		}
	}

	// decrypt and recover
	slice.Decrypt(resp.shards)
	err := slice.Reconstruct(resp.shards)
	if err != nil {
		return nil, err
	}

	return resp.shards, err
}

func (mgr *downloadManager) Stats() downloadManagerStats {
	// recompute stats
	mgr.tryRecomputeStats()

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	// collect stats
	stats := make(map[types.PublicKey]downloaderStats)
	for hk, d := range mgr.downloaders {
		stats[hk] = d.stats()
	}

	return downloadManagerStats{
		avgDownloadSpeedMBPS: mgr.statsSlabDownloadSpeedBytesPerMS.Average() * 0.008, // convert bytes per ms to mbps,
		avgOverdrivePct:      mgr.statsOverdrivePct.Average(),
		downloaders:          stats,
	}
}

func (mgr *downloadManager) Stop() {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	close(mgr.stopChan)
	for _, d := range mgr.downloaders {
		close(d.stopChan)
	}
}

func (mgr *downloadManager) tryRecomputeStats() {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if time.Since(mgr.lastRecompute) < statsRecomputeMinInterval {
		return
	}

	for _, d := range mgr.downloaders {
		d.statsSectorDownloadEstimateInMS.Recompute()
		d.statsDownloadSpeedBytesPerMS.Recompute()
	}
	mgr.lastRecompute = time.Now()
}

func (mgr *downloadManager) numDownloaders() int {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return len(mgr.downloaders)
}

func (mgr *downloadManager) refreshDownloaders(contracts []api.ContractMetadata) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	// build map
	want := make(map[types.PublicKey]api.ContractMetadata)
	for _, c := range contracts {
		want[c.HostKey] = c
	}

	// prune downloaders
	for hk := range mgr.downloaders {
		_, wanted := want[hk]
		if !wanted {
			close(mgr.downloaders[hk].stopChan)
			delete(mgr.downloaders, hk)
			continue
		}

		delete(want, hk) // remove from want so remainging ones are the missing ones
	}

	// update downloaders
	for _, c := range want {
		// create a host
		host := mgr.hp.newHostV3(c.ID, c.HostKey, c.SiamuxAddr)
		downloader := newDownloader(host)
		mgr.downloaders[c.HostKey] = downloader
		go downloader.processQueue(mgr.hp)
	}
}

func (mgr *downloadManager) newSlabDownload(ctx context.Context, dID id, slice object.SlabSlice, slabIndex int) (*slabDownload, func()) {
	// create slab id
	var sID slabID
	frand.Read(sID[:])

	// add slab to ongoing downloads
	mgr.mu.Lock()
	mgr.ongoing[sID] = struct{}{}
	mgr.mu.Unlock()

	// prepare a function to remove it from the ongoing downloads
	finishFn := func() {
		mgr.mu.Lock()
		delete(mgr.ongoing, sID)
		mgr.mu.Unlock()
	}

	// calculate the offset and length
	offset, length := slice.SectorRegion()

	// build sector info
	hostToSectors := make(map[types.PublicKey][]sectorInfo)
	for sI, s := range slice.Shards {
		hostToSectors[s.Host] = append(hostToSectors[s.Host], sectorInfo{s, sI})
	}

	// create slab download
	return &slabDownload{
		mgr: mgr,

		dID:       dID,
		sID:       sID,
		created:   time.Now(),
		index:     slabIndex,
		minShards: int(slice.MinShards),
		offset:    offset,
		length:    length,

		hostToSectors: hostToSectors,
		used:          make(map[types.PublicKey]struct{}),

		sectors: make([][]byte, len(slice.Shards)),
	}, finishFn
}

func (mgr *downloadManager) ongoingDownloads() int {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return len(mgr.ongoing)
}

func (mgr *downloadManager) downloadSlab(ctx context.Context, dID id, slice object.SlabSlice, index int, responseChan chan *slabDownloadResponse, nextSlabChan chan struct{}) {
	// add tracing
	ctx, span := tracing.Tracer.Start(ctx, "downloadSlab")
	defer span.End()

	// prepare the download
	slab, finishFn := mgr.newSlabDownload(ctx, dID, slice, index)
	defer finishFn()

	// download shards
	resp := &slabDownloadResponse{index: index}
	resp.shards, resp.err = slab.downloadShards(ctx, nextSlabChan)

	// check if we're done first
	select {
	case <-ctx.Done():
		return
	default:
		// if not try and send the response
		select {
		case <-ctx.Done():
		case responseChan <- resp:
		}
	}
}

func (d *downloader) stats() downloaderStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return downloaderStats{
		avgSpeedMBPS: d.statsDownloadSpeedBytesPerMS.Average() * 0.008,
		healthy:      d.consecutiveFailures == 0,
		numDownloads: d.numDownloads,
	}
}

func (d *downloader) isStopped() bool {
	select {
	case <-d.stopChan:
		return true
	default:
	}
	return false
}

func (d *downloader) fillBatch() (batch []*sectorDownloadReq) {
	for len(batch) < maxConcurrentSectorsPerHost {
		if req := d.pop(); req == nil {
			break
		} else if req.done() {
			continue
		} else {
			batch = append(batch, req)
		}
	}
	return
}

func (d *downloader) processBatch(batch []*sectorDownloadReq) chan struct{} {
	doneChan := make(chan struct{})

	// define some state to keep track of stats
	var mu sync.Mutex
	var start time.Time
	var concurrent int64
	var downloadedB int64
	trackStatsFn := func() {
		if start.IsZero() || time.Since(start).Milliseconds() == 0 || downloadedB == 0 {
			return
		}
		durationMS := time.Since(start).Milliseconds()
		d.statsDownloadSpeedBytesPerMS.Track(float64(downloadedB / durationMS))
		d.statsSectorDownloadEstimateInMS.Track(float64(durationMS))
		start = time.Time{}
		downloadedB = 0
	}

	// define a worker to process download requests
	inflight := uint64(len(batch))
	reqsChan := make(chan *sectorDownloadReq)
	workerFn := func() {
		for req := range reqsChan {
			if d.isStopped() {
				break
			}

			// update state
			mu.Lock()
			if start.IsZero() {
				start = time.Now()
			}
			concurrent++
			mu.Unlock()

			// execute the request
			err := d.execute(req)
			d.trackFailure(err)

			// update state + potentially track stats
			mu.Lock()
			if err == nil {
				downloadedB += int64(req.length) + downloadOverheadB
				if downloadedB >= maxConcurrentSectorsPerHost*rhpv2.SectorSize || concurrent == maxConcurrentSectorsPerHost {
					trackStatsFn()
				}
			}
			concurrent--
			if concurrent < 0 {
				panic("concurrent can never be less than zero") // developer error
			}
			mu.Unlock()
		}

		// last worker that's done closes the channel and flushes the stats
		if atomic.AddUint64(&inflight, ^uint64(0)) == 0 {
			close(doneChan)
			trackStatsFn()
		}
	}

	// launch workers
	for i := 0; i < len(batch); i++ {
		go workerFn()
	}
	for _, req := range batch {
		reqsChan <- req
	}

	// launch a goroutine to keep the request coming
	go func() {
		defer close(reqsChan)
		for {
			if req := d.pop(); req == nil {
				break
			} else if req.done() {
				continue
			} else {
				reqsChan <- req
			}
		}
	}()

	return doneChan
}

func (d *downloader) processQueue(hp hostProvider) {
outer:
	for {
		// wait for work
		select {
		case <-d.signalWorkChan:
		case <-d.stopChan:
			return
		}

		for {
			// try fill a batch of requests
			batch := d.fillBatch()
			if len(batch) == 0 {
				continue outer
			}

			// process the batch
			doneChan := d.processBatch(batch)
			for {
				select {
				case <-d.stopChan:
					return
				case <-doneChan:
					continue outer
				}
			}
		}
	}
}

func (d *downloader) estimate() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()

	// fetch estimated duration per sector
	estimateP90 := d.statsSectorDownloadEstimateInMS.P90()
	if estimateP90 == 0 {
		if avg := d.statsSectorDownloadEstimateInMS.Average(); avg > 0 {
			estimateP90 = avg
		} else {
			estimateP90 = 1
		}
	}

	numSectors := float64(len(d.queue) + 1)
	return numSectors * estimateP90
}

func (d *downloader) enqueue(download *sectorDownloadReq) {
	// add tracing
	span := trace.SpanFromContext(download.ctx)
	span.SetAttributes(attribute.Float64("estimate", d.estimate()))
	span.AddEvent("enqueued")

	// enqueue the job
	d.mu.Lock()
	d.queue = append(d.queue, download)
	d.mu.Unlock()

	// signal there's work
	select {
	case d.signalWorkChan <- struct{}{}:
	default:
	}
}

func (d *downloader) pop() *sectorDownloadReq {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.queue) > 0 {
		j := d.queue[0]
		d.queue[0] = nil
		d.queue = d.queue[1:]
		return j
	}
	return nil
}

func (d *downloader) trackFailure(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err == nil {
		d.consecutiveFailures = 0
		return
	}

	if isBalanceInsufficient(err) ||
		isPriceTableExpired(err) ||
		isPriceTableNotFound(err) ||
		isSectorNotFound(err) {
		return // host is not to blame for these errors
	}

	d.consecutiveFailures++
	d.statsSectorDownloadEstimateInMS.Track(float64(time.Hour.Milliseconds()))
}

func (d *downloader) execute(req *sectorDownloadReq) (err error) {
	// add tracing
	start := time.Now()
	span := trace.SpanFromContext(req.ctx)
	span.AddEvent("execute")
	defer func() {
		elapsed := time.Since(start)
		span.SetAttributes(attribute.Int64("duration", elapsed.Milliseconds()))
		span.RecordError(err)
		span.End()
	}()

	// download the sector
	buf := bytes.NewBuffer(make([]byte, 0, rhpv2.SectorSize))
	err = d.host.DownloadSector(req.ctx, buf, req.root, req.offset, req.length)
	if err != nil {
		req.fail(err)
		return err
	}

	d.mu.Lock()
	d.numDownloads++
	d.mu.Unlock()

	req.succeed(buf.Bytes())
	return nil
}

func (req *sectorDownloadReq) succeed(sector []byte) {
	select {
	case <-req.ctx.Done():
	case req.responseChan <- sectorDownloadResp{
		hk:          req.hk,
		overdrive:   req.overdrive,
		sectorIndex: req.sectorIndex,
		sector:      sector,
	}:
	}
}

func (req *sectorDownloadReq) fail(err error) {
	select {
	case <-req.ctx.Done():
	case req.responseChan <- sectorDownloadResp{
		err:       err,
		hk:        req.hk,
		overdrive: req.overdrive,
	}:
	}
}

func (req *sectorDownloadReq) done() bool {
	select {
	case <-req.ctx.Done():
		return true
	default:
		return false
	}
}

func (s *slabDownload) overdrive(ctx context.Context, respChan chan sectorDownloadResp) (resetTimer func()) {
	// overdrive is disabled
	if s.mgr.overdriveTimeout == 0 {
		return func() {}
	}

	// create a helper function that increases the timeout for each overdrive
	timeout := func() time.Duration {
		s.mu.Lock()
		defer s.mu.Unlock()
		return time.Duration(s.numOverdriving+1) * s.mgr.overdriveTimeout
	}

	// create a timer to trigger overdrive
	timer := time.NewTimer(timeout())
	resetTimer = func() {
		timer.Stop()
		select {
		case <-timer.C:
		default:
		}
		timer.Reset(timeout())
	}

	// create a function to check whether overdrive is possible
	canOverdrive := func(timeout time.Duration) bool {
		s.mu.Lock()
		defer s.mu.Unlock()

		// overdrive is not due yet
		if time.Since(s.lastOverdrive) < timeout {
			return false
		}

		// overdrive is maxed out
		remaining := s.minShards - s.numCompleted
		if s.numInflight >= s.mgr.maxOverdrive+uint64(remaining) {
			return false
		}

		s.lastOverdrive = time.Now()
		return true
	}

	// try overdriving every time the timer fires
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if canOverdrive(timeout()) {
					_ = s.launch(s.nextRequest(ctx, respChan, true)) // ignore error
				}
				resetTimer()
			}
		}
	}()

	return
}

func (s *slabDownload) nextRequest(ctx context.Context, responseChan chan sectorDownloadResp, overdrive bool) *sectorDownloadReq {
	s.mu.Lock()
	defer s.mu.Unlock()

	// prepare next sectors to download
	if len(s.hostToSectors[s.curr]) == 0 {
		// grab unused hosts
		var hosts []types.PublicKey
		for host := range s.hostToSectors {
			if _, used := s.used[host]; !used {
				hosts = append(hosts, host)
			}
		}

		// make the fastest host the current host
		s.curr = s.mgr.fastest(hosts)
		s.used[s.curr] = struct{}{}

		// no more sectors to download
		if len(s.hostToSectors[s.curr]) == 0 {
			return nil
		}
	}

	// pop the next sector
	sector := s.hostToSectors[s.curr][0]
	s.hostToSectors[s.curr] = s.hostToSectors[s.curr][1:]

	// create the span
	sCtx, span := tracing.Tracer.Start(ctx, "sectorDownloadReq")
	span.SetAttributes(attribute.Stringer("hk", sector.Host))
	span.SetAttributes(attribute.Bool("overdrive", overdrive))
	span.SetAttributes(attribute.Int("sector", sector.index))

	// build the request
	return &sectorDownloadReq{
		ctx: sCtx,

		offset: s.offset,
		length: s.length,
		root:   sector.Root,
		hk:     sector.Host,

		overdrive:    overdrive,
		sectorIndex:  sector.index,
		responseChan: responseChan,
	}
}

func (s *slabDownload) downloadShards(ctx context.Context, nextSlabTrigger chan struct{}) ([][]byte, error) {
	// cancel any sector downloads once the download is done
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// add tracing
	ctx, span := tracing.Tracer.Start(ctx, "downloadShards")
	defer span.End()

	// create the response channel
	respChan := make(chan sectorDownloadResp)

	// launch overdrive
	resetOverdrive := s.overdrive(ctx, respChan)

	// launch 'MinShard' requests
	for i := 0; i < int(s.minShards); i++ {
		req := s.nextRequest(ctx, respChan, false)
		if err := s.launch(req); err != nil {
			return nil, errors.New("no hosts available")
		}
	}

	// collect responses
	var done bool
	var next bool
	var triggered bool
	for s.inflight() > 0 && !done {
		var resp sectorDownloadResp
		select {
		case <-s.mgr.stopChan:
			return nil, errors.New("download stopped")
		case <-ctx.Done():
			return nil, ctx.Err()
		case resp = <-respChan:
		}

		resetOverdrive()

		done, next = s.receive(resp)
		if !done && resp.err != nil {
			_ = s.launch(s.nextRequest(ctx, respChan, true)) // ignore error
		}

		if next && !triggered && s.mgr.ongoingDownloads() < maxConcurrentSlabsPerDownload {
			select {
			case nextSlabTrigger <- struct{}{}:
				triggered = true
			default:
			}
		}
	}

	// make sure next slab is triggered
	if done && !triggered {
		select {
		case nextSlabTrigger <- struct{}{}:
		default:
		}
	}

	// track stats
	s.mgr.statsOverdrivePct.Track(s.overdrivePct())
	s.mgr.statsSlabDownloadSpeedBytesPerMS.Track(float64(s.downloadSpeed()))
	return s.finish()
}

func (s *slabDownload) overdrivePct() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	numOverdrive := int(s.numLaunched) - s.minShards
	if numOverdrive < 0 {
		numOverdrive = 0
	}

	return float64(numOverdrive) / float64(s.minShards)
}

func (s *slabDownload) downloadSpeed() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	completedShards := len(s.sectors)
	bytes := completedShards * rhpv2.SectorSize
	ms := time.Since(s.created).Milliseconds()
	return int64(bytes) / ms
}

func (s *slabDownload) finish() ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.numCompleted < s.minShards {
		return nil, fmt.Errorf("failed to download slab: completed=%d, inflight=%d, launched=%d downloaders=%d errors=%w", s.numCompleted, s.numInflight, s.numLaunched, s.mgr.numDownloaders(), s.errs)
	}
	return s.sectors, nil
}

func (s *slabDownload) inflight() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.numInflight
}

func (s *slabDownload) launch(req *sectorDownloadReq) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// check for nil
	if req == nil {
		return errors.New("no request given")
	}

	// launch the req
	err := s.mgr.launch(req)
	if err != nil {
		span := trace.SpanFromContext(req.ctx)
		span.RecordError(err)
		span.End()
		return err
	}

	// update the state
	s.numInflight++
	s.numLaunched++
	if req.overdrive {
		s.numOverdriving++
	}
	return nil
}

func (s *slabDownload) receive(resp sectorDownloadResp) (finished bool, next bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// update num overdriving
	if resp.overdrive {
		s.numOverdriving--
	}

	// failed reqs can't complete the upload
	s.numInflight--
	if resp.err != nil {
		s.errs = append(s.errs, &HostError{resp.hk, resp.err})
		return false, false
	}

	// store the sector
	s.sectors[resp.sectorIndex] = resp.sector
	s.numCompleted++

	return s.numCompleted >= s.minShards, s.numCompleted+int(s.mgr.maxOverdrive) >= s.minShards
}

func (mgr *downloadManager) fastest(hosts []types.PublicKey) (fastest types.PublicKey) {
	// recompute stats
	mgr.tryRecomputeStats()

	// return the fastest host
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	lowest := math.MaxFloat64
	for _, h := range hosts {
		if d, ok := mgr.downloaders[h]; !ok {
			continue
		} else if estimate := d.estimate(); estimate < lowest {
			lowest = estimate
			fastest = h
		}
	}
	return
}

func (mgr *downloadManager) launch(req *sectorDownloadReq) error {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	downloader, exists := mgr.downloaders[req.hk]
	if !exists {
		return fmt.Errorf("no downloader for host %v", req.hk)
	}

	downloader.enqueue(req)
	return nil
}

func newID() id {
	var id id
	frand.Read(id[:])
	return id
}

func (id id) String() string {
	return fmt.Sprintf("%x", id[:])
}

func slabsForDownload(slabs []object.SlabSlice, offset, length uint64) []object.SlabSlice {
	// declare a helper to cast a uint64 to uint32 with overflow detection. This
	// could should never produce an overflow.
	cast32 := func(in uint64) uint32 {
		if in > math.MaxUint32 {
			panic("slabsForDownload: overflow detected")
		}
		return uint32(in)
	}

	// mutate a copy
	slabs = append([]object.SlabSlice(nil), slabs...)

	firstOffset := offset
	for i, ss := range slabs {
		if firstOffset <= uint64(ss.Length) {
			slabs = slabs[i:]
			break
		}
		firstOffset -= uint64(ss.Length)
	}
	slabs[0].Offset += cast32(firstOffset)
	slabs[0].Length -= cast32(firstOffset)

	lastLength := length
	for i, ss := range slabs {
		if lastLength <= uint64(ss.Length) {
			slabs = slabs[:i+1]
			break
		}
		lastLength -= uint64(ss.Length)
	}
	slabs[len(slabs)-1].Length = cast32(lastLength)
	return slabs
}
