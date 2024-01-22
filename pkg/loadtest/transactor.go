package loadtest

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sagaxyz/tm-load-test/internal/logging"
)

const (
	connSendTimeout = 10 * time.Second
	// see https://github.com/tendermint/tendermint/blob/v0.32.x/rpc/lib/server/handlers.go
	connPingPeriod = (30 * 9 / 10) * time.Second

	jsonRPCID = -1

	defaultProgressCallbackInterval = 5 * time.Second
)

// Transactor represents a single wire-level connection to a Tendermint RPC
// endpoint, and this is responsible for sending transactions to that endpoint.
type Transactor struct {
	remoteAddr string  // The full URL of the remote WebSockets endpoint.
	config     *Config // The configuration for the load test.

	client            Client
	logger            logging.Logger
	conn              *websocket.Conn
	broadcastTxMethod string
	wg                sync.WaitGroup

	// Rudimentary statistics
	statsMtx  sync.RWMutex
	startTime time.Time // When did the transaction sending start?
	txCount   int       // How many transactions have been sent.
	txBytes   int64     // How many transaction bytes have been sent, cumulatively.
	txRate    float64   // The number of transactions sent, per second.

	descL []*loadDesc
	stats *ProcessedStats

	progressCallbackMtx      sync.RWMutex
	progressCallbackID       int                                      // A unique identifier for this transactor when calling the progress callback.
	progressCallbackInterval time.Duration                            // How frequently to call the progress update callback.
	progressCallback         func(id int, txCount int, txBytes int64) // Called with the total number of transactions executed so far.

	stopMtx sync.RWMutex
	stop    bool
	stopErr error // Did an error occur that triggered the stop?
}

// NewTransactor initiates a WebSockets connection to the given host address.
// Must be a valid WebSockets URL, e.g. "ws://host:port/websocket"
func NewTransactor(remoteAddr string, config *Config) (*Transactor, error) {
	u, err := url.Parse(remoteAddr)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("unsupported protocol: %s (only ws:// and wss:// are supported)", u.Scheme)
	}
	clientFactory, exists := clientFactories[config.ClientFactory]
	if !exists {
		return nil, fmt.Errorf("unrecognized client factory: %s", config.ClientFactory)
	}
	client, err := clientFactory.NewClient(*config)
	if err != nil {
		return nil, err
	}
	conn, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to connect to remote WebSockets endpoint %s: %s (status code %d)", remoteAddr, resp.Status, resp.StatusCode)
	}
	logger := logging.NewLogrusLogger(fmt.Sprintf("transactor[%s]", u.String()))
	logger.Info("Connected to remote Tendermint WebSockets RPC")
	return &Transactor{
		remoteAddr:               u.String(),
		config:                   config,
		client:                   client,
		logger:                   logger,
		conn:                     conn,
		broadcastTxMethod:        "broadcast_tx_" + config.BroadcastTxMethod,
		progressCallbackInterval: defaultProgressCallbackInterval,
		descL:                    make([]*loadDesc, 0, 100),
	}, nil
}

func (t *Transactor) SetProgressCallback(id int, interval time.Duration, callback func(int, int, int64)) {
	t.progressCallbackMtx.Lock()
	t.progressCallbackID = id
	t.progressCallbackInterval = interval
	t.progressCallback = callback
	t.progressCallbackMtx.Unlock()
}

// Start kicks off the transactor's operations in separate goroutines (one for
// reading from the WebSockets endpoint, and one for writing to it).
func (t *Transactor) Start() {
	t.logger.Debug("Starting transactor")
	t.wg.Add(2)
	go t.receiveLoop()
	go t.sendLoop()
}

// Cancel will indicate to the transactor that it must stop, but does not wait
// until it has completely stopped. To wait, call the Transactor.Wait() method.
func (t *Transactor) Cancel() {
	t.setStop(fmt.Errorf("transactor operations cancelled"))
}

// Wait will block until the transactor terminates.
func (t *Transactor) Wait() error {
	t.wg.Wait()
	t.stopMtx.RLock()
	defer t.stopMtx.RUnlock()
	return t.stopErr
}

// GetTxCount returns the total number of transactions sent thus far by this
// transactor.
func (t *Transactor) GetTxCount() int {
	t.statsMtx.RLock()
	defer t.statsMtx.RUnlock()
	return t.txCount
}

// GetTxBytes returns the cumulative total number of bytes (as transactions)
// sent thus far by this transactor.
func (t *Transactor) GetTxBytes() int64 {
	t.statsMtx.RLock()
	defer t.statsMtx.RUnlock()
	return t.txBytes
}

// GetTxRate returns the average number of transactions per second sent by
// this transactor over the duration of its operation.
func (t *Transactor) GetTxRate() float64 {
	t.statsMtx.RLock()
	defer t.statsMtx.RUnlock()
	return t.txRate
}

func (t *Transactor) receiveLoop() {
	defer t.wg.Done()
	for {
		// right now we don't care about what we read back from the RPC endpoint
		_, reply, err := t.conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				t.logger.Error("Failed to read response on connection", "err", err)
				return
			}
		}
		if t.mustStop() {
			return
		}
		var response RPCResponse
		err = json.Unmarshal(reply, &response)
		if err != nil {
			t.logger.Error("Error detected", "err", err)
		}
		if response.Error != nil {
			t.logger.Error("Error detected in response message", "err", response.Error.Message)
		}
	}
}

func (t *Transactor) processStats() {
	defer func() {
		t.descL = t.descL[:0]
	}()

	// 1. Sort the values by .Latency
	sort.Slice(t.descL, func(i, j int) bool {
		di, dj := t.descL[i], t.descL[j]
		return di.Latency < dj.Latency
	})

	// Values are sorted by latency, but still bucketize them by the second of occurrence.
	buckets := make(map[int][]*loadDesc, len(t.descL))

	startTime := t.startTime
	var maxTimeDur time.Duration = -10
	for _, ds := range t.descL {
		timeDiff := ds.At.Sub(startTime)
		if timeDiff > maxTimeDur {
			maxTimeDur = timeDiff
		}
		indexBySecond := int(math.Floor(timeDiff.Seconds()))
		buckets[indexBySecond] = append(buckets[indexBySecond], ds)

	}

	// For each second, bucket values for latency and bytes processed.

	var totalTxs uint64
	bucketized := make([]*BucketizedBySecond, 0, len(buckets))
	totalBytes := uint64(0)
	for sec := 0; sec < len(buckets); sec++ {
		values := buckets[sec]

		// 1. Rank by latency.
		sort.Slice(values, func(i, j int) bool {
			vi, vj := values[i], values[j]
			return vi.Latency < vj.Latency
		})
		latencyRankings := &ProcessedStats{
			P50thLatency: t.pNthForLatency(values, 50),
			P75thLatency: t.pNthForLatency(values, 75),
			P90thLatency: t.pNthForLatency(values, 90),
			P95thLatency: t.pNthForLatency(values, 95),
			P99thLatency: t.pNthForLatency(values, 99),
		}

		// 2. Rank by bytes.
		sort.Slice(values, func(i, j int) bool {
			vi, vj := values[i], values[j]
			return vi.Size < vj.Size
		})
		bytesRankings := &ProcessedStats{
			P50thLatency: t.pNthForBytes(values, 50),
			P75thLatency: t.pNthForBytes(values, 75),
			P90thLatency: t.pNthForBytes(values, 90),
			P95thLatency: t.pNthForBytes(values, 95),
			P99thLatency: t.pNthForBytes(values, 99),
		}

		bytesPerSecond := int(0)
		totalTxs += uint64(len(values))
		for _, di := range values {
			bytesPerSecond += di.Size
		}
		totalBytes += uint64(bytesPerSecond)
		bucketized = append(bucketized, &BucketizedBySecond{
			Sec:   sec,
			QPS:   len(values),
			Bytes: bytesPerSecond,

			LatencyRankings: latencyRankings,
			BytesRankings:   bytesRankings,
		})
	}

	raw := make([]*loadDesc, len(t.descL))
	copy(raw, t.descL)
	raw = nil

	t.stats = &ProcessedStats{
		AvgBytesPerSecond: float64(totalBytes) / maxTimeDur.Seconds(),
		AvgTxPerSecond:    float64(totalTxs) / maxTimeDur.Seconds(),

		PerSecond:  bucketized,
		TotalBytes: totalBytes,
		TotalTxs:   totalTxs,
		TotalTime:  maxTimeDur,
		StartTime:  &startTime,

		P50thLatency: t.pNth(t.descL, 50),
		P75thLatency: t.pNth(t.descL, 75),
		P90thLatency: t.pNth(t.descL, 90),
		P95thLatency: t.pNth(t.descL, 95),
		P99thLatency: t.pNth(t.descL, 99),

		Raw: raw,
	}
}

type DescPercentile struct {
	AtNs    int64         `json:"at_ns,omitempty"`
	AtStr   string        `json:"at_str,omitempty"`
	Latency time.Duration `json:"latency,omitempty"`
	Size    int           `json:"size,omitempty"`
}

type BucketizedBySecond struct {
	Sec   int `json:"sec"`
	QPS   int `json:"qps,omitempty"`
	Bytes int `json:"bytes,omitempty"`

	BytesRankings   *ProcessedStats `json:"bytes_rankings,omitempty"`
	LatencyRankings *ProcessedStats `json:"latency_rankings,omitempty"`
}

type ProcessedStats struct {
	AvgBytesPerSecond float64       `json:"avg_bytes_per_sec,omitempty"`
	AvgTxPerSecond    float64       `json:"avg_tx_per_sec,omitempty"`
	TotalTime         time.Duration `json:"total_time,omitempty"`
	TotalBytes        uint64        `json:"total_bytes,omitempty"`
	TotalTxs          uint64        `json:"total_txs,omitempty"`

	P50thLatency *DescPercentile `json:"p50,omitempty"`
	P75thLatency *DescPercentile `json:"p75,omitempty"`
	P90thLatency *DescPercentile `json:"p90,omitempty"`
	P95thLatency *DescPercentile `json:"p95,omitempty"`
	P99thLatency *DescPercentile `json:"p99,omitempty"`

	PerSecond []*BucketizedBySecond `json:"per_sec,omitempty"`

	StartTime *time.Time  `json:"start_time,omitempty"`
	Raw       []*loadDesc `json:"-"`

	Rankings []*ProcessedStats `json:"rankings,omitempty"`
}

func (t *Transactor) pNthForBytes(descL []*loadDesc, nth int) *DescPercentile {
	dp := t.pNth(descL, nth)
	if dp != nil {
		dp.Latency = 0
	}
	return dp
}

func (t *Transactor) pNthForLatency(descL []*loadDesc, nth int) *DescPercentile {
	dp := t.pNth(descL, nth)
	if dp != nil {
		dp.Size = 0
	}
	return dp
}

func (t *Transactor) pNth(descL []*loadDesc, nth int) *DescPercentile {
	if len(descL) == 0 {
		return nil
	}

	i := int(float64(nth*len(descL)) / 100.0)
	di := descL[i]
	at := di.At.Sub(t.startTime)
	return &DescPercentile{
		AtNs:    at.Nanoseconds(),
		AtStr:   at.String(),
		Latency: di.Latency,
		Size:    di.Size,
	}
}

func (t *Transactor) sendLoop() {
	defer t.wg.Done()
	t.conn.SetPingHandler(func(message string) error {
		err := t.conn.WriteControl(websocket.PongMessage, []byte(message), time.Now().Add(connSendTimeout))
		if err == websocket.ErrCloseSent {
			return nil
		}
		return err
	})

	pingTicker := time.NewTicker(connPingPeriod)
	timeLimitTicker := time.NewTicker(time.Duration(t.config.Time) * time.Second)
	sendTicker := time.NewTicker(time.Duration(t.config.SendPeriod) * time.Second)
	progressTicker := time.NewTicker(t.getProgressCallbackInterval())
	defer func() {
		pingTicker.Stop()
		timeLimitTicker.Stop()
		sendTicker.Stop()
		progressTicker.Stop()
	}()

	defer t.processStats()

	for {
		if t.config.Count > 0 && t.GetTxCount() >= t.config.Count {
			t.logger.Info("Maximum transaction limit reached", "count", t.GetTxCount())
			t.setStop(nil)
		}
		select {
		case <-sendTicker.C:
			if err := t.sendTransactions(); err != nil {
				t.logger.Error("Failed to send transactions", "err", err)
				t.setStop(err)
			}

		case <-progressTicker.C:
			t.reportProgress()

		case <-pingTicker.C:
			if err := t.sendPing(); err != nil {
				t.logger.Error("Failed to write ping message", "err", err)
				t.setStop(err)
			}

		case <-timeLimitTicker.C:
			t.logger.Info("Time limit reached for load testing")
			t.setStop(nil)
		}
		if t.mustStop() {
			t.close()
			return
		}
	}
}

func (t *Transactor) writeTx(tx []byte) error {
	txHex := "0x" + hex.EncodeToString(tx)
	paramsJSON, err := json.Marshal([]string{txHex})
	if err != nil {
		return err
	}
	_ = t.conn.SetWriteDeadline(time.Now().Add(connSendTimeout))
	return t.conn.WriteJSON(RPCRequest{
		JSONRPC: "2.0",
		ID:      jsonRPCID,
		Method:  "eth_sendRawTransaction",
		Params:  json.RawMessage(paramsJSON),
	})
}

func (t *Transactor) mustStop() bool {
	t.stopMtx.RLock()
	defer t.stopMtx.RUnlock()
	return t.stop
}

func (t *Transactor) setStop(err error) {
	t.stopMtx.Lock()
	t.stop = true
	if err != nil {
		t.stopErr = err
	}
	t.stopMtx.Unlock()
}

type loadDesc struct {
	At      time.Time
	Size    int
	Latency time.Duration
}

func (t *Transactor) sendTransactions() error {
	// send as many transactions as we can, up to the send rate
	totalSent := t.GetTxCount()
	toSend := t.config.Rate
	if (t.config.Count > 0) && ((totalSent + toSend) > t.config.Count) {
		toSend = t.config.Count - totalSent
		t.logger.Debug("Nearing max transaction count", "totalSent", totalSent, "maxTxCount", t.config.Count, "toSend", toSend)
	}
	if totalSent == 0 {
		t.trackStartTime()
	}

	var sent int
	var sentBytes int64
	defer func() { t.trackSentTxs(sent, sentBytes) }()
	t.logger.Info("Sending batch of transactions", "toSend", toSend)
	batchStartTime := time.Now()
	for ; sent < toSend; sent++ {
		tx, err := t.client.GenerateTx()
		if err != nil {
			return err
		}

		tStart := time.Now()
		if err := t.writeTx(tx); err != nil {
			return err
		}
		t.descL = append(t.descL, &loadDesc{At: tStart, Size: len(tx), Latency: time.Since(tStart)})
		sentBytes += int64(len(tx))
		// if we have to make way for the next batch
		if time.Since(batchStartTime) >= time.Duration(t.config.SendPeriod)*time.Second {
			break
		}
	}
	return nil
}

func (t *Transactor) trackStartTime() {
	t.statsMtx.Lock()
	t.startTime = time.Now()
	t.txRate = 0.0
	t.statsMtx.Unlock()
}

func (t *Transactor) trackSentTxs(count int, byteCount int64) {
	t.statsMtx.Lock()
	defer t.statsMtx.Unlock()

	t.txCount += count
	t.txBytes += byteCount
	elapsed := time.Since(t.startTime).Seconds()
	if elapsed > 0 {
		t.txRate = float64(t.txCount) / elapsed
	} else {
		t.txRate = 0
	}
}

func (t *Transactor) sendPing() error {
	_ = t.conn.SetWriteDeadline(time.Now().Add(connSendTimeout))
	return t.conn.WriteMessage(websocket.PingMessage, []byte{})
}

func (t *Transactor) reportProgress() {
	txCount := t.GetTxCount()
	txRate := t.GetTxRate()
	txBytes := t.GetTxBytes()
	t.logger.Debug("Statistics", "txCount", txCount, "txRate", fmt.Sprintf("%.3f txs/sec", txRate))

	t.progressCallbackMtx.RLock()
	defer t.progressCallbackMtx.RUnlock()
	if t.progressCallback != nil {
		t.progressCallback(t.progressCallbackID, txCount, txBytes)
	}
}

func (t *Transactor) getProgressCallbackInterval() time.Duration {
	t.progressCallbackMtx.RLock()
	defer t.progressCallbackMtx.RUnlock()
	return t.progressCallbackInterval
}

func (t *Transactor) close() {
	// try to cleanly shut down the connection
	_ = t.conn.SetWriteDeadline(time.Now().Add(connSendTimeout))
	err := t.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	if err != nil {
		t.logger.Error("Failed to write close message", "err", err)
	} else {
		t.logger.Debug("Wrote close message to remote endpoint")
	}
}
