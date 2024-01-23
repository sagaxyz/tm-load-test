package loadtest

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"math/rand"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/sirupsen/logrus"
)

// EvmClientFactory creates instances of EvmClient
type EvmClientFactory struct {
	mainPrivKey    *ecdsa.PrivateKey
	mainAddress    common.Address
	erc20Address   common.Address
	erc721Address  common.Address
	erc1155Address common.Address
}

const ERC20abi = "[{\"constant\":false,\"inputs\":[{\"name\":\"_to\",\"type\":\"address\"},{\"name\":\"_value\",\"type\":\"uint256\"}],\"name\":\"transfer\",\"outputs\":[{\"name\":\"\",\"type\":\"bool\"}],\"type\":\"function\"}]"
const ERC721abi = "[{\"constant\":false,\"inputs\":[{\"name\":\"player\",\"type\":\"address\"},{\"name\":\"tokenURI\",\"type\":\"string\"}],\"name\":\"awardItem\",\"outputs\":[],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"}]"
const ERC1155abi = "[{\"constant\":false,\"inputs\":[{\"name\":\"from\",\"type\":\"address\"},{\"name\":\"to\",\"type\":\"address\"},{\"name\":\"id\",\"type\":\"uint256\"},{\"name\":\"amount\",\"type\":\"uint256\"},{\"name\":\"data\",\"type\":\"bytes\"}],\"name\":\"safeTransferFrom\",\"outputs\":[],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"}]"

// EvmClient is responsible for generating transactions. Only one client
// will be created per connection to the remote Tendermint RPC endpoint, and
// each client will be responsible for maintaining its own state in a
// thread-safe manner.
type EvmClient struct {
	privateKey     *ecdsa.PrivateKey
	address        common.Address
	nonce          uint64
	gasPrice       *big.Int
	networkId      *big.Int
	client         *ethclient.Client
	erc20Address   common.Address
	erc721Address  common.Address
	erc1155Address common.Address
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

	erc20AddressEnvVar := "ERC20_ADDRESS"
	erc20Address := os.Getenv(erc20AddressEnvVar)
	if erc20Address == "" {
		logrus.Errorf("environment variable %s is not set", erc20AddressEnvVar)
		return nil
	}

	erc721AddressEnvVar := "ERC721_ADDRESS"
	erc721Address := os.Getenv(erc721AddressEnvVar)
	if erc721Address == "" {
		logrus.Errorf("environment variable %s is not set", erc721AddressEnvVar)
		return nil
	}

	erc1155AddressEnvVar := "ERC1155_ADDRESS"
	erc1155Address := os.Getenv(erc1155AddressEnvVar)
	if erc1155Address == "" {
		logrus.Errorf("environment variable %s is not set", erc1155AddressEnvVar)
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
		mainPrivKey:    privateKey,
		mainAddress:    address,
		erc20Address:   common.HexToAddress(erc20Address),
		erc721Address:  common.HexToAddress(erc721Address),
		erc1155Address: common.HexToAddress(erc1155Address),
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

	// send ERC20 tokens
	contractABI, err := abi.JSON(strings.NewReader(ERC20abi))
	if err != nil {
		return nil, fmt.Errorf("unable to get abi: %v", err)
	}
	contractInstance := bind.NewBoundContract(f.erc20Address, contractABI, client, client, client)
	if err != nil {
		return nil, fmt.Errorf("unable to create contract instance: %v", err)
	}
	auth, err := bind.NewKeyedTransactorWithChainID(f.mainPrivKey, chainID)
	if err != nil {
		return nil, fmt.Errorf("unable to create keyed transactor: %v", err)
	}

	tokensToSend := big.NewInt(1000000000)
	_, err = contractInstance.Transact(auth, "transfer", addrNew, tokensToSend)
	if err != nil {
		return nil, fmt.Errorf("unable to transfer tokens to a new client %s: %v", addrNew.String(), err)
	}

	// send some of ERC1155 tokens
	contractABI, err = abi.JSON(strings.NewReader(ERC1155abi))
	if err != nil {
		return nil, fmt.Errorf("unable to get abi: %v", err)
	}
	contractInstance = bind.NewBoundContract(f.erc1155Address, contractABI, client, client, client)
	if err != nil {
		return nil, fmt.Errorf("unable to create ERC1155 contract instance: %v", err)
	}

	tokenId := big.NewInt(1)
	_, err = contractInstance.Transact(auth, "safeTransferFrom", f.mainAddress, addrNew, tokenId, tokensToSend, []byte{})
	if err != nil {
		return nil, fmt.Errorf("unable to transfer ERC1155 tokens to a new client %s: %v", addrNew.String(), err)
	}

	time.Sleep(5 * time.Second)

	newAccNonce, err := client.PendingNonceAt(context.Background(), addrNew)
	if err != nil {
		return nil, fmt.Errorf("unable to get nonce for new account: %v", err)
	}
	logrus.Infof("new acc nonce: %d, gas price: %d", newAccNonce, gasPrice)

	return &EvmClient{
		privateKey:     privateKey,
		address:        addrNew,
		nonce:          newAccNonce,
		gasPrice:       gasPrice,
		networkId:      chainID,
		client:         client,
		erc20Address:   f.erc20Address,
		erc721Address:  f.erc721Address,
		erc1155Address: f.erc1155Address,
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

	value := big.NewInt(1)
	var tx []byte

	r := rand.Int() % 4
	switch r {
	case 0: // normal tokens
		tx, err = c.prepareNormalTx(addrNew, value)
	case 1: // erc20
		tx, err = c.prepareSmartContractTx("transfer", ERC20abi, c.erc20Address, addrNew, value)
	case 2: // erc721
		tx, err = c.prepareSmartContractTx("awardItem", ERC721abi, c.erc721Address, addrNew, "1.json")
	case 3: // erc1155
		tx, err = c.prepareSmartContractTx("safeTransferFrom", ERC1155abi, c.erc1155Address, c.address, addrNew, big.NewInt(1), value, []byte{})
	default:
		panic("Should never happen")
	}

	if err != nil {
		return nil, fmt.Errorf("error happenned while generating tx: %v", err)
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
	c.nonce++

	return tx, nil
}

func (c *EvmClient) prepareNormalTx(to common.Address, value *big.Int) ([]byte, error) {
	gasLimit := uint64(21000)

	gasprice, err := c.client.SuggestGasPrice(context.Background())
	if err != nil {
		return nil, fmt.Errorf("unable to calculate gas price: %v", err)
	}
	tx := types.NewTransaction(c.nonce, to, value, gasLimit, gasprice, nil)
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(c.networkId), c.privateKey)
	if err != nil {
		return nil, fmt.Errorf("cannot sign tx: %v", err)
	}

	bytesData, err := signedTx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("unable to marshal tx: %v", err)
	}

	return bytesData, nil
}

func (c *EvmClient) prepareSmartContractTx(functionName, abiStr string, contractAddress common.Address, args ...interface{}) ([]byte, error) {
	contractABI, err := abi.JSON(strings.NewReader(abiStr))
	if err != nil {
		return nil, fmt.Errorf("unable to get abi: %v", err)
	}
	data, err := contractABI.Pack(functionName, args...)
	if err != nil {
		return nil, fmt.Errorf("unable to pack arguments: %v", err)
	}

	gasPrice, err := c.client.SuggestGasPrice(context.Background())
	if err != nil {
		return nil, fmt.Errorf("unable to calculate gas price: %v", err)
	}
	gasLimit := uint64(200000)
	tx := types.NewTransaction(c.nonce, contractAddress, big.NewInt(0), gasLimit, gasPrice, data)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(c.networkId), c.privateKey)
	if err != nil {
		return nil, fmt.Errorf("cannot sign tx: %v", err)
	}

	bytesData, err := signedTx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("unable to marshal tx: %v", err)
	}

	return bytesData, nil
}
