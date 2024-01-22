package loadtest

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/sirupsen/logrus"
)

// EvmClientFactory creates instances of EvmClient
type EvmClientFactory struct {
	mainPrivKey *ecdsa.PrivateKey
	mainAddress common.Address
}

// EvmClient is responsible for generating transactions. Only one client
// will be created per connection to the remote Tendermint RPC endpoint, and
// each client will be responsible for maintaining its own state in a
// thread-safe manner.
type EvmClient struct {
	privateKey *ecdsa.PrivateKey
	address    common.Address
	nonce      uint64
	gasPrice   *big.Int
	networkId  *big.Int
	client     *ethclient.Client
}

var (
	_ ClientFactory = (*EvmClientFactory)(nil)
	_ Client        = (*EvmClient)(nil)
)

func init() {
	if err := RegisterClientFactory("evm", NewEvmClientFactory()); err != nil {
		panic(err)
	}
}

func NewEvmClientFactory() *EvmClientFactory {
	// this key should have non-zero balance
	keyEnvVar := "MAIN_PRIV_KEY_HEX"
	mainPrivKeyHex := os.Getenv(keyEnvVar)
	if mainPrivKeyHex == "" {
		logrus.Errorf("environment variable %s is not set", keyEnvVar)
		return nil
	}

	privateKey, err := crypto.HexToECDSA(mainPrivKeyHex)
	if err != nil {
		logrus.Errorf("unable to get ECDSA private key: %v", err)
		return nil
	}

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		logrus.Error("error casting public key to ECDSA")
		return nil
	}
	address := crypto.PubkeyToAddress(*publicKeyECDSA)
	logrus.Infof("main address: %s", address.String())

	return &EvmClientFactory{
		mainPrivKey: privateKey,
		mainAddress: address,
	}
}

func (f *EvmClientFactory) ValidateConfig(cfg Config) error {
	// Do any checks here that you need to ensure that the load test
	// configuration is compatible with your client.
	return nil
}

func (f *EvmClientFactory) NewClient(cfg Config) (Client, error) {
	// create new account
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("unable to generate ECDSA private key: %v", err)
	}

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("error casting public key to ECDSA")
	}
	addrNew := crypto.PubkeyToAddress(*publicKeyECDSA)
	logrus.Infof("generated new client address: %s", addrNew.Hex())

	wsUrl, err := url.Parse(cfg.Endpoints[0])
	if err != nil {
		return nil, fmt.Errorf("unable to parse url: %v", err)
	}
	logrus.Infof("ws url: %s", wsUrl.String())

	client, err := ethclient.Dial(wsUrl.String())
	if err != nil {
		logrus.Info("error")
		return nil, fmt.Errorf("unable to dial: %v", err)
	}

	balance, err := client.BalanceAt(context.Background(), f.mainAddress, nil)
	if err != nil {
		return nil, fmt.Errorf("unable to get balance: %v", err)
	}
	logrus.Infof("main balance: %s", balance.String())

	nonce, err := client.PendingNonceAt(context.Background(), f.mainAddress)
	if err != nil {
		return nil, fmt.Errorf("unable to get nonce: %v", err)
	}

	// generate tx and send funds to a new account
	value := big.NewInt(1000000000000000000) // in wei (1 eth)
	gasLimit := uint64(21000)

	gasPrice, err := client.SuggestGasPrice(context.Background())
	if err != nil {
		return nil, fmt.Errorf("unable to calculate gas price: %v", err)
	}

	tx := types.NewTransaction(nonce, addrNew, value, gasLimit, gasPrice, nil)

	// send tx
	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("unable to get network id: %v", err)
	}

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), f.mainPrivKey)
	if err != nil {
		return nil, fmt.Errorf("cannot sign tx: %v", err)
	}

	err = client.SendTransaction(context.Background(), signedTx)
	if err != nil {
		return nil, fmt.Errorf("unable to send transaction: %v", err)
	}

	time.Sleep(5 * time.Second)

	newAccNonce, err := client.PendingNonceAt(context.Background(), addrNew)
	if err != nil {
		return nil, fmt.Errorf("unable to get nonce for new account: %v", err)
	}
	logrus.Infof("new acc nonce: %d, gas price: %d", newAccNonce, gasPrice)

	return &EvmClient{
		privateKey: privateKey,
		address:    addrNew,
		nonce:      newAccNonce,
		gasPrice:   gasPrice,
		networkId:  chainID,
		client:     client,
	}, nil
}

// GenerateTx must return the raw bytes that make up the transaction for your
// ABCI app. The conversion to base64 will automatically be handled by the
// loadtest package, so don't worry about that. Only return an error here if you
// want to completely fail the entire load test operation.
func (c *EvmClient) GenerateTx() ([]byte, error) {
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("unable to generate ECDSA private key: %v", err)
	}

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("error casting public key to ECDSA")
	}
	addrNew := crypto.PubkeyToAddress(*publicKeyECDSA)
	logrus.Debugf("generated new random address: %s", addrNew.Hex())

	// generate tx and send funds to a new account
	value := big.NewInt(10)   // in wei
	gasLimit := uint64(21000) // in units

	gasprice, err := c.client.SuggestGasPrice(context.Background())
	if err != nil {
		return nil, fmt.Errorf("unable to calculate gas price: %v", err)
	}

	// if for some reason nonce desyncs, we need to update it
	// calling PendingNonceAt every time does not work too well
	// since it does not count transactions which are not yet pending
	const nonceVerifyPeriod = 100
	if c.nonce%nonceVerifyPeriod == 0 {
		newNonce, err := c.client.PendingNonceAt(context.Background(), c.address)
		if err != nil {
			return nil, fmt.Errorf("unable to get nonce: %v", err)
		}
		c.nonce = newNonce
	}

	tx := types.NewTransaction(c.nonce, addrNew, value, gasLimit, gasprice, nil)

	// send tx
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(c.networkId), c.privateKey)
	if err != nil {
		return nil, fmt.Errorf("cannot sign tx: %v", err)
	}

	bytesData, err := signedTx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("unable to marshal tx: %v", err)
	}

	c.nonce++

	return bytesData, nil
}
