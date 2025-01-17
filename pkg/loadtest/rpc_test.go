package loadtest_test

import (
	"encoding/json"
	"testing"

	"github.com/sagaxyz/tm-load-test/pkg/loadtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetInfoDeserialization(t *testing.T) {
	tc := `{
  "id": 0,
  "jsonrpc": "2.0",
  "result": {
    "listening": true,
    "listeners": [
      "Listener(@)"
    ],
    "n_peers": "1",
    "peers": [
      {
        "node_info": {
          "protocol_version": {
            "p2p": "7",
            "block": "10",
            "app": "0"
          },
          "id": "5576458aef205977e18fd50b274e9b5d9014525a",
          "listen_addr": "tcp:0.0.0.0:26656",
          "network": "cosmoshub-2",
          "version": "0.32.1",
          "channels": "4020212223303800",
          "moniker": "moniker-node",
          "other": {
            "tx_index": "on",
            "rpc_address": "tcp:0.0.0.0:26657"
          }
        },
        "is_outbound": true,
        "connection_status": {
          "Duration": "168901057956119",
          "SendMonitor": {
            "Active": true,
            "Start": "2019-07-31T14:31:28.66Z",
            "Duration": "168901060000000",
            "Idle": "168901040000000",
            "Bytes": "5",
            "Samples": "1",
            "InstRate": "0",
            "CurRate": "0",
            "AvgRate": "0",
            "PeakRate": "0",
            "BytesRem": "0",
            "TimeRem": "0",
            "Progress": 0
          },
          "RecvMonitor": {
            "Active": true,
            "Start": "2019-07-31T14:31:28.66Z",
            "Duration": "168901060000000",
            "Idle": "168901040000000",
            "Bytes": "5",
            "Samples": "1",
            "InstRate": "0",
            "CurRate": "0",
            "AvgRate": "0",
            "PeakRate": "0",
            "BytesRem": "0",
            "TimeRem": "0",
            "Progress": 0
          },
          "Channels": [
            {
              "ID": 48,
              "SendQueueCapacity": "1",
              "SendQueueSize": "0",
              "Priority": "5",
              "RecentlySent": "0"
            }
          ]
        },
        "remote_ip": "95.179.155.35"
      }
    ]
  }
}`
	res := &loadtest.RPCResponse{}
	err := json.Unmarshal([]byte(tc), res)
	require.NoError(t, err)

	netInfo := &loadtest.NetInfo{}
	err = json.Unmarshal(res.Result, netInfo)
	require.NoError(t, err)
	assert.Equal(t, 1, int(netInfo.NPeers))
	assert.Len(t, netInfo.Peers, 1)
}
