package loadtest

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"math/rand"
	"net/url"
	"os"
	"strings"
	"sync"
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
	mainPrivKey       *ecdsa.PrivateKey
	mainAddress       common.Address
	erc20Address      common.Address
	erc721Address     common.Address
	erc1155Address    common.Address
	seededAccounts    []account
	preseededAccounts []account
	startFrom         int
	m                 sync.Mutex
}

const ERC20abi = "[{\"constant\":false,\"inputs\":[{\"name\":\"_to\",\"type\":\"address\"},{\"name\":\"_value\",\"type\":\"uint256\"}],\"name\":\"transfer\",\"outputs\":[{\"name\":\"\",\"type\":\"bool\"}],\"type\":\"function\"}]"
const ERC721abi = "[{\"constant\":false,\"inputs\":[{\"name\":\"player\",\"type\":\"address\"},{\"name\":\"tokenURI\",\"type\":\"string\"}],\"name\":\"awardItem\",\"outputs\":[],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"}]"
const ERC1155abi = "[{\"constant\":false,\"inputs\":[{\"name\":\"from\",\"type\":\"address\"},{\"name\":\"to\",\"type\":\"address\"},{\"name\":\"id\",\"type\":\"uint256\"},{\"name\":\"amount\",\"type\":\"uint256\"},{\"name\":\"data\",\"type\":\"bytes\"}],\"name\":\"safeTransferFrom\",\"outputs\":[],\"payable\":false,\"stateMutability\":\"nonpayable\",\"type\":\"function\"}]"

// EvmClient is responsible for generating transactions. Only one client
// will be created per connection to the remote Tendermint RPC endpoint, and
// each client will be responsible for maintaining its own state in a
// thread-safe manner.
type EvmClient struct {
	// privateKey     *ecdsa.PrivateKey
	// address        common.Address
	// nonce          uint64
	accounts        []account
	networkId       *big.Int
	erc20Address    common.Address
	erc721Address   common.Address
	erc1155Address  common.Address
	lastAccountUsed int
}

var (
	_ ClientFactory = (*EvmClientFactory)(nil)
	_ Client        = (*EvmClient)(nil)
)

var (
	client               *ethclient.Client
	numAccountsPerClient = 1000
	numPreseededAccounts = 10
	keysFileName         = "keys.dat"
)

func init() {
	if err := RegisterClientFactory("evm", NewEvmClientFactory()); err != nil {
		panic(err)
	}
}

func loadKeys() []account {
	logrus.Infof("Preloading keys from %s", keysFileName)
	keyFile, err := os.Open(keysFileName)
	if err != nil {
		logrus.Warnf("unable to open %s: %v", keysFileName, err)
		return nil
	}
	defer keyFile.Close()
	fileScanner := bufio.NewScanner(keyFile)
	fileScanner.Split(bufio.ScanLines)

	accounts := make([]account, 0)

	for fileScanner.Scan() {
		privateKeyHex := fileScanner.Text()
		privateKey, err := crypto.HexToECDSA(privateKeyHex)
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
		logrus.Infof("loaded key %s", address.String())

		newAccount := account{
			privateKey: privateKey,
			address:    address,
			nonce:      0,
		}
		accounts = append(accounts, newAccount)
	}

	return accounts
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

	seededAccs := loadKeys()
	preseededAccs := make([]account, 0)

	return &EvmClientFactory{
		mainPrivKey:       privateKey,
		mainAddress:       address,
		erc20Address:      common.HexToAddress(erc20Address),
		erc721Address:     common.HexToAddress(erc721Address),
		erc1155Address:    common.HexToAddress(erc1155Address),
		seededAccounts:    seededAccs,
		preseededAccounts: preseededAccs,
		startFrom:         0,
	}
}

func (f *EvmClientFactory) ValidateConfig(cfg Config) error {
	// Do any checks here that you need to ensure that the load test
	// configuration is compatible with your client.
	return nil
}

type account struct {
	privateKey *ecdsa.PrivateKey
	address    common.Address
	nonce      uint64
}

func (f *EvmClientFactory) seedAccount(value, chainID *big.Int, seedingKey *ecdsa.PrivateKey, nonce uint64, accounts *[]account) error {
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		return fmt.Errorf("unable to generate ECDSA private key: %v", err)
	}

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("error casting public key to ECDSA")
	}
	addrNew := crypto.PubkeyToAddress(*publicKeyECDSA)
	logrus.Infof("generated new client address: %s", addrNew.Hex())

	balance, err := client.BalanceAt(context.Background(), f.mainAddress, nil)
	if err != nil {
		return fmt.Errorf("unable to get balance: %v", err)
	}
	logrus.Infof("main balance: %s", balance.String())

	// generate tx and send funds to a new account
	gasLimit := uint64(21000)
	gasPrice := big.NewInt(1000000000)

	tx := types.NewTransaction(nonce, addrNew, value, gasLimit, gasPrice, nil)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), seedingKey)
	if err != nil {
		return fmt.Errorf("cannot sign tx: %v", err)
	}

	err = client.SendTransaction(context.Background(), signedTx)
	if err != nil {
		return fmt.Errorf("unable to send transaction: %v", err)
	}

	// send ERC20 tokens
	contractABI, err := abi.JSON(strings.NewReader(ERC20abi))
	if err != nil {
		return fmt.Errorf("unable to get abi: %v", err)
	}
	contractInstance := bind.NewBoundContract(f.erc20Address, contractABI, client, client, client)
	auth, err := bind.NewKeyedTransactorWithChainID(f.mainPrivKey, chainID)
	if err != nil {
		return fmt.Errorf("unable to create keyed transactor: %v", err)
	}
	auth.GasLimit = 200000

	tokensToSend := big.NewInt(1000000000)
	_, err = contractInstance.Transact(auth, "transfer", addrNew, tokensToSend)
	if err != nil {
		return fmt.Errorf("unable to transfer tokens to a new client %s: %v", addrNew.String(), err)
	}

	// send some of ERC1155 tokens
	contractABI, err = abi.JSON(strings.NewReader(ERC1155abi))
	if err != nil {
		return fmt.Errorf("unable to get abi: %v", err)
	}
	contractInstance = bind.NewBoundContract(f.erc1155Address, contractABI, client, client, client)
	tokenId := big.NewInt(1)
	_, err = contractInstance.Transact(auth, "safeTransferFrom", f.mainAddress, addrNew, tokenId, tokensToSend, []byte{})
	if err != nil {
		return fmt.Errorf("unable to transfer ERC1155 tokens to a new client %s: %v", addrNew.String(), err)
	}
	// _, err = bind.WaitMined(context.Background(), client, erc1155tx)
	// if err != nil {
	// 	return nil, fmt.Errorf("WaitMined failed: %v", err)
	// }

	// newNonce, err := client.PendingNonceAt(context.Background(), addrNew)
	// if err != nil {
	// 	return nil, fmt.Errorf("unable to get nonce: %v", err)
	// }

	newAcc := account{
		privateKey: privateKey,
		address:    addrNew,
	}

	f.m.Lock()
	defer f.m.Unlock()
	*accounts = append(*accounts, newAcc)
	return nil
}

func (f *EvmClientFactory) NewClient(cfg Config) (Client, error) {
	if client == nil {
		wsUrl, err := url.Parse(cfg.Endpoints[0])
		if err != nil {
			return nil, fmt.Errorf("unable to parse url: %v", err)
		}
		logrus.Infof("ws url: %s", wsUrl.String())

		client, err = ethclient.Dial(wsUrl.String())
		if err != nil {
			logrus.Info("error")
			return nil, fmt.Errorf("unable to dial: %v", err)
		}
	}

	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("unable to get network id: %v", err)
	}

	if f.startFrom+numAccountsPerClient <= len(f.seededAccounts) {
		logrus.Infof("reusing %d seeded accounts", numAccountsPerClient)
		reusedAccs := f.seededAccounts[f.startFrom : f.startFrom+numAccountsPerClient]
		f.startFrom += numAccountsPerClient

		for i, acc := range reusedAccs {
			logrus.Infof("getting nonce for account %s", acc.address)
			newNonce, err := client.PendingNonceAt(context.Background(), acc.address)
			if err != nil {
				return nil, fmt.Errorf("unable to get nonce: %v", err)
			}
			reusedAccs[i].nonce = newNonce
		}

		logrus.Info("got all nonces")

		return &EvmClient{
			accounts:        reusedAccs,
			networkId:       chainID,
			erc20Address:    f.erc20Address,
			erc721Address:   f.erc721Address,
			erc1155Address:  f.erc1155Address,
			lastAccountUsed: 0,
		}, nil
	}

	if len(f.preseededAccounts) == 0 {
		value, b := new(big.Int).SetString("1000000000000000000000", 10) // in wei (1000 eth)
		if !b {
			return nil, fmt.Errorf("Unable to convert string to big.Int")
		}
		for i := 0; i < numPreseededAccounts; i++ {
			nonce, err := client.PendingNonceAt(context.Background(), f.mainAddress)
			if err != nil {
				return nil, fmt.Errorf("unable to get nonce: %v", err)
			}
			err = f.seedAccount(value, chainID, f.mainPrivKey, nonce, &f.preseededAccounts)
			if err != nil {
				return nil, fmt.Errorf("Unable to seed account: %v", err)
			}
		}
		time.Sleep(5 * time.Second)
		for _, acc := range f.preseededAccounts {
			balance, err := client.BalanceAt(context.Background(), acc.address, nil)
			if err != nil {
				return nil, fmt.Errorf("unable to get balance: %v", err)
			}
			logrus.Infof("seed account %s balance: %s", acc.address, balance.String())
		}
	}

	logrus.Infof("Preseeded accounts: %d", len(f.preseededAccounts))

	// create new accounts
	keysFile, err := os.OpenFile(keysFileName, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("unable to open file: %v", err)
	}
	defer keysFile.Close()

	// nonce, err := client.PendingNonceAt(context.Background(), f.mainAddress)
	// if err != nil {
	// 	return nil, fmt.Errorf("unable to get nonce: %v", err)
	// }

	accounts := make([]account, 0)
	value, b := new(big.Int).SetString("1000000000000000000", 10) // in wei (1 eth)
	if !b {
		return nil, fmt.Errorf("Unable to convert string to big.Int")
	}

	for i := range f.preseededAccounts {
		nonce, err := client.PendingNonceAt(context.Background(), f.preseededAccounts[i].address)
		if err != nil {
			return nil, fmt.Errorf("unable to get nonce: %v", err)
		}
		f.preseededAccounts[i].nonce = nonce
	}

	for i := 0; i < numAccountsPerClient; i += numPreseededAccounts {
		var wg sync.WaitGroup
		for j := 0; j < numPreseededAccounts; j++ {
			seedingKey := f.preseededAccounts[j].privateKey
			seedingAddress := f.preseededAccounts[j].address
			wg.Add(1)
			go func() {
				defer wg.Done()
				var (
					accounts *[]account = &accounts
				)
				privateKey, err := crypto.GenerateKey()
				if err != nil {
					logrus.Errorf("unable to generate ECDSA private key: %v", err)
				}
				publicKey := privateKey.Public()
				publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
				if !ok {
					logrus.Errorf("error casting public key to ECDSA")
				}
				addrNew := crypto.PubkeyToAddress(*publicKeyECDSA)
				logrus.Infof("generated new client address: %s", addrNew.Hex())
				nonce, err := client.PendingNonceAt(context.Background(), seedingAddress)
				if err != nil {
					logrus.Errorf("unable to get nonce: %v", err)
				}
				gasLimit := uint64(21000)
				gasPrice := big.NewInt(10000000)
				tx := types.NewTransaction(nonce, addrNew, value, gasLimit, gasPrice, nil)
				signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), seedingKey)
				if err != nil {
					logrus.Errorf("cannot sign tx: %v", err)
				}
				err = client.SendTransaction(context.Background(), signedTx)
				if err != nil {
					logrus.Errorf("unable to send transaction: %v", err)
				}
				contractABI, err := abi.JSON(strings.NewReader(ERC20abi))
				if err != nil {
					logrus.Errorf("unable to get abi: %v", err)
				}
				contractInstance := bind.NewBoundContract(f.erc20Address, contractABI, client, client, client)
				auth, err := bind.NewKeyedTransactorWithChainID(seedingKey, chainID)
				auth.GasLimit = 200000
				if err != nil {
					logrus.Errorf("unable to create keyed transactor: %v", err)
				}
				tokensToSend := big.NewInt(100000)
				resTx, err := contractInstance.Transact(auth, "transfer", addrNew, tokensToSend)
				if err != nil {
					logrus.Errorf("unable to transfer tokens to a new client %s: %v", addrNew.String(), err)
				}
				_, _ = bind.WaitMined(context.Background(), client, resTx)
				contractABI, err = abi.JSON(strings.NewReader(ERC1155abi))
				if err != nil {
					logrus.Errorf("unable to get abi: %v", err)
				}
				contractInstance = bind.NewBoundContract(f.erc1155Address, contractABI, client, client, client)
				if err != nil {
					logrus.Errorf("unable to create ERC1155 contract instance: %v", err)
				}
				tokenId := big.NewInt(1)
				resTx, err = contractInstance.Transact(auth, "safeTransferFrom", seedingAddress, addrNew, tokenId, tokensToSend, []byte{})
				if err != nil {
					logrus.Errorf("unable to transfer ERC1155 tokens to a new client %s: %v", addrNew.String(), err)
				}
				_, _ = bind.WaitMined(context.Background(), client, resTx)
				newAcc := account{privateKey: privateKey, address: addrNew}
				f.m.Lock()
				defer f.m.Unlock()
				*accounts = append(*accounts, newAcc)
			}()
		}
		wg.Wait()
	}
	for _, acc := range accounts {
		hexKey := fmt.Sprintf("%064s", acc.privateKey.D.Text(16))
		logrus.Info(hexKey)
		_, err = keysFile.WriteString(hexKey + "\n")
		if err != nil {
			return nil, fmt.Errorf("unable to append new account to %s: %v", keysFileName, err)
		}
		// nonce += 3
	}

	return &EvmClient{
		accounts:        accounts,
		networkId:       chainID,
		erc20Address:    f.erc20Address,
		erc721Address:   f.erc721Address,
		erc1155Address:  f.erc1155Address,
		lastAccountUsed: 0,
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
	logrus.Debugf("generated new address: %s", addrNew.Hex())

	// if for some reason nonce desyncs, we need to update it
	// calling PendingNonceAt every time does not work too well
	// since it does not count transactions which are not yet pending

	// newNonce, err := client.PendingNonceAt(context.Background(), c.accounts[accIndex].address)
	// if err != nil {
	// 	return nil, fmt.Errorf("unable to get nonce: %v", err)
	// }
	// c.accounts[accIndex].nonce = newNonce

	const nonceVerifyPeriod = 10
	if c.accounts[c.lastAccountUsed].nonce%nonceVerifyPeriod == 0 {
		logrus.Infof("updating nonce for client %s", c.accounts[c.lastAccountUsed].address)
		newNonce, err := client.PendingNonceAt(context.Background(), c.accounts[c.lastAccountUsed].address)
		if err != nil {
			return nil, fmt.Errorf("unable to get nonce: %v", err)
		}
		c.accounts[c.lastAccountUsed].nonce = newNonce
	}

	value := big.NewInt(1)
	var tx []byte

	// r := rand.Int() % 4
	payload := randSeq(500)
	r := 2
	switch r {
	case 0: // normal tokens
		tx, err = c.prepareNormalTx(c.accounts[c.lastAccountUsed], addrNew, value)
	case 1: // erc20
		tx, err = c.prepareSmartContractTx(c.accounts[c.lastAccountUsed], "transfer", ERC20abi, c.erc20Address, uint64(70000), addrNew, value)
	case 2: // erc721
		tx, err = c.prepareSmartContractTx(c.accounts[c.lastAccountUsed], "awardItem", ERC721abi, c.erc721Address, uint64(500000), addrNew, payload)
	case 3: // erc1155
		tx, err = c.prepareSmartContractTx(c.accounts[c.lastAccountUsed], "safeTransferFrom", ERC1155abi, c.erc1155Address, uint64(70000), c.accounts[c.lastAccountUsed].address, addrNew, big.NewInt(1), value, []byte{})
	default:
		panic("Should never happen")
	}

	c.accounts[c.lastAccountUsed].nonce++

	if err != nil {
		return nil, fmt.Errorf("error happenned while generating tx: %v", err)
	}

	c.lastAccountUsed++
	if c.lastAccountUsed == numAccountsPerClient {
		c.lastAccountUsed = 0
	}

	return tx, nil
}

func (c *EvmClient) prepareNormalTx(acc account, to common.Address, value *big.Int) ([]byte, error) {
	gasLimit := uint64(21000)
	// newNonce, err := client.PendingNonceAt(context.Background(), acc.address)
	// if err != nil {
	// 	return nil, fmt.Errorf("unable to get nonce: %v", err)
	// }
	gasprice := big.NewInt(100)
	// gasprice, err := client.SuggestGasPrice(context.Background())
	// if err != nil {
	// 	return nil, fmt.Errorf("unable to calculate gas price: %v", err)
	// }
	// logrus.Infof("gas price: %s", gasprice.String())
	tx := types.NewTransaction(acc.nonce, to, value, gasLimit, gasprice, nil)
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(c.networkId), acc.privateKey)
	if err != nil {
		return nil, fmt.Errorf("cannot sign tx: %v", err)
	}
	logrus.Infof("normal tx hash: %s", signedTx.Hash())

	bytesData, err := signedTx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("unable to marshal tx: %v", err)
	}

	// logrus.Infof("normal tx size: %d", len(bytesData))

	return bytesData, nil
}

func (c *EvmClient) prepareSmartContractTx(acc account, functionName, abiStr string, contractAddress common.Address, gasLimit uint64, args ...interface{}) ([]byte, error) {
	contractABI, err := abi.JSON(strings.NewReader(abiStr))
	if err != nil {
		return nil, fmt.Errorf("unable to get abi: %v", err)
	}
	data, err := contractABI.Pack(functionName, args...)
	if err != nil {
		return nil, fmt.Errorf("unable to pack arguments: %v", err)
	}

	// newNonce, err := client.PendingNonceAt(context.Background(), acc.address)
	// if err != nil {
	// 	return nil, fmt.Errorf("unable to get nonce: %v", err)
	// }
	gasPrice := big.NewInt(100)
	// gasPrice, err := client.SuggestGasPrice(context.Background())
	// if err != nil {
	// 	return nil, fmt.Errorf("unable to calculate gas price: %v", err)
	// }
	tx := types.NewTransaction(acc.nonce, contractAddress, big.NewInt(0), gasLimit, gasPrice, data)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(c.networkId), acc.privateKey)
	if err != nil {
		return nil, fmt.Errorf("cannot sign tx: %v", err)
	}

	logrus.Infof("contract tx hash: %s", signedTx.Hash())

	bytesData, err := signedTx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("unable to marshal tx: %v", err)
	}

	logrus.Infof("smart contract (%s) tx size: %d", functionName, len(bytesData))
	return bytesData, nil
}

func randSeq(n int) string {
	var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	b := make([]rune, n)
	for i := range b {
		b[i] = letters[rand.Int()%len(letters)]
	}
	return string(b)
}
