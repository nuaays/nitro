// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package arbtest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/offchainlabs/nitro/arbnode/execution"
	"github.com/offchainlabs/nitro/arbos/arbostypes"
	"github.com/offchainlabs/nitro/arbos/util"
	"github.com/offchainlabs/nitro/arbstate"
	"github.com/offchainlabs/nitro/blsSignatures"
	"github.com/offchainlabs/nitro/cmd/chaininfo"
	"github.com/offchainlabs/nitro/cmd/genericconf"
	"github.com/offchainlabs/nitro/das"
	"github.com/offchainlabs/nitro/util/arbmath"
	"github.com/offchainlabs/nitro/util/headerreader"
	"github.com/offchainlabs/nitro/util/signature"
	"github.com/offchainlabs/nitro/validator/server_api"
	"github.com/offchainlabs/nitro/validator/server_common"
	"github.com/offchainlabs/nitro/validator/valnode"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/offchainlabs/nitro/arbnode"
	"github.com/offchainlabs/nitro/arbos"
	"github.com/offchainlabs/nitro/arbutil"
	_ "github.com/offchainlabs/nitro/nodeInterface"
	"github.com/offchainlabs/nitro/solgen/go/bridgegen"
	"github.com/offchainlabs/nitro/solgen/go/mocksgen"
	"github.com/offchainlabs/nitro/solgen/go/precompilesgen"
	"github.com/offchainlabs/nitro/statetransfer"
	"github.com/offchainlabs/nitro/util/testhelpers"
)

type info = *BlockchainTestInfo
type client = arbutil.L1Interface

func SendWaitTestTransactions(t *testing.T, ctx context.Context, client client, txs []*types.Transaction) {
	t.Helper()
	for _, tx := range txs {
		Require(t, client.SendTransaction(ctx, tx))
	}
	for _, tx := range txs {
		_, err := EnsureTxSucceeded(ctx, client, tx)
		Require(t, err)
	}
}

func TransferBalance(
	t *testing.T, from, to string, amount *big.Int, l2info info, client client, ctx context.Context,
) (*types.Transaction, *types.Receipt) {
	t.Helper()
	return TransferBalanceTo(t, from, l2info.GetAddress(to), amount, l2info, client, ctx)
}

func TransferBalanceTo(
	t *testing.T, from string, to common.Address, amount *big.Int, l2info info, client client, ctx context.Context,
) (*types.Transaction, *types.Receipt) {
	t.Helper()
	tx := l2info.PrepareTxTo(from, &to, l2info.TransferGas, amount, nil)
	err := client.SendTransaction(ctx, tx)
	Require(t, err)
	res, err := EnsureTxSucceeded(ctx, client, tx)
	Require(t, err)
	return tx, res
}

// if l2client is not nil - will wait until balance appears in l2
func BridgeBalance(
	t *testing.T, account string, amount *big.Int, l1info info, l2info info, l1client client, l2client client, ctx context.Context,
) (*types.Transaction, *types.Receipt) {
	t.Helper()

	// setup or validate the same account on l2info
	l1acct := l1info.GetInfoWithPrivKey(account)
	if l2info.Accounts[account] == nil {
		l2info.SetFullAccountInfo(account, &AccountInfo{
			Address:    l1acct.Address,
			PrivateKey: l1acct.PrivateKey,
			Nonce:      0,
		})
	} else {
		l2acct := l2info.GetInfoWithPrivKey(account)
		if l2acct.PrivateKey.X.Cmp(l1acct.PrivateKey.X) != 0 ||
			l2acct.PrivateKey.Y.Cmp(l1acct.PrivateKey.Y) != 0 {
			Fatal(t, "l2 account already exists and not compatible to l1")
		}
	}

	// check previous balance
	var l2Balance *big.Int
	var err error
	if l2client != nil {
		l2Balance, err = l2client.BalanceAt(ctx, l2info.GetAddress("Faucet"), nil)
		Require(t, err)
	}

	// send transaction
	data, err := hex.DecodeString("0f4d14e9000000000000000000000000000000000000000000000000000082f79cd90000")
	Require(t, err)
	tx := l1info.PrepareTx(account, "Inbox", l1info.TransferGas*100, amount, data)
	err = l1client.SendTransaction(ctx, tx)
	Require(t, err)
	res, err := EnsureTxSucceeded(ctx, l1client, tx)
	Require(t, err)

	// wait for balance to appear in l2
	if l2client != nil {
		l2Balance.Add(l2Balance, amount)
		for i := 0; true; i++ {
			balance, err := l2client.BalanceAt(ctx, l2info.GetAddress("Faucet"), nil)
			Require(t, err)
			if balance.Cmp(l2Balance) >= 0 {
				break
			}
			TransferBalance(t, "Faucet", "User", big.NewInt(1), l1info, l1client, ctx)
			if i > 20 {
				Fatal(t, "bridging failed")
			}
			<-time.After(time.Millisecond * 100)
		}
	}

	return tx, res
}

func SendSignedTxViaL1(
	t *testing.T,
	ctx context.Context,
	l1info *BlockchainTestInfo,
	l1client arbutil.L1Interface,
	l2client arbutil.L1Interface,
	delayedTx *types.Transaction,
) *types.Receipt {
	delayedInboxContract, err := bridgegen.NewInbox(l1info.GetAddress("Inbox"), l1client)
	Require(t, err)
	usertxopts := l1info.GetDefaultTransactOpts("User", ctx)

	txbytes, err := delayedTx.MarshalBinary()
	Require(t, err)
	txwrapped := append([]byte{arbos.L2MessageKind_SignedTx}, txbytes...)
	l1tx, err := delayedInboxContract.SendL2Message(&usertxopts, txwrapped)
	Require(t, err)
	_, err = EnsureTxSucceeded(ctx, l1client, l1tx)
	Require(t, err)

	// sending l1 messages creates l1 blocks.. make enough to get that delayed inbox message in
	for i := 0; i < 30; i++ {
		SendWaitTestTransactions(t, ctx, l1client, []*types.Transaction{
			l1info.PrepareTx("Faucet", "Faucet", 30000, big.NewInt(1e12), nil),
		})
	}
	receipt, err := EnsureTxSucceeded(ctx, l2client, delayedTx)
	Require(t, err)
	return receipt
}

func SendUnsignedTxViaL1(
	t *testing.T,
	ctx context.Context,
	l1info *BlockchainTestInfo,
	l1client arbutil.L1Interface,
	l2client arbutil.L1Interface,
	templateTx *types.Transaction,
) *types.Receipt {
	delayedInboxContract, err := bridgegen.NewInbox(l1info.GetAddress("Inbox"), l1client)
	Require(t, err)

	usertxopts := l1info.GetDefaultTransactOpts("User", ctx)
	remapped := util.RemapL1Address(usertxopts.From)
	nonce, err := l2client.NonceAt(ctx, remapped, nil)
	Require(t, err)

	unsignedTx := types.NewTx(&types.ArbitrumUnsignedTx{
		ChainId:   templateTx.ChainId(),
		From:      remapped,
		Nonce:     nonce,
		GasFeeCap: templateTx.GasFeeCap(),
		Gas:       templateTx.Gas(),
		To:        templateTx.To(),
		Value:     templateTx.Value(),
		Data:      templateTx.Data(),
	})

	l1tx, err := delayedInboxContract.SendUnsignedTransaction(
		&usertxopts,
		arbmath.UintToBig(unsignedTx.Gas()),
		unsignedTx.GasFeeCap(),
		arbmath.UintToBig(unsignedTx.Nonce()),
		*unsignedTx.To(),
		unsignedTx.Value(),
		unsignedTx.Data(),
	)
	Require(t, err)
	_, err = EnsureTxSucceeded(ctx, l1client, l1tx)
	Require(t, err)

	// sending l1 messages creates l1 blocks.. make enough to get that delayed inbox message in
	for i := 0; i < 30; i++ {
		SendWaitTestTransactions(t, ctx, l1client, []*types.Transaction{
			l1info.PrepareTx("Faucet", "Faucet", 30000, big.NewInt(1e12), nil),
		})
	}
	receipt, err := EnsureTxSucceeded(ctx, l2client, unsignedTx)
	Require(t, err)
	return receipt
}

func GetBaseFee(t *testing.T, client client, ctx context.Context) *big.Int {
	header, err := client.HeaderByNumber(ctx, nil)
	Require(t, err)
	return header.BaseFee
}

func GetBaseFeeAt(t *testing.T, client client, ctx context.Context, blockNum *big.Int) *big.Int {
	header, err := client.HeaderByNumber(ctx, blockNum)
	Require(t, err)
	return header.BaseFee
}

type lifecycle struct {
	start func() error
	stop  func() error
}

func (l *lifecycle) Start() error {
	if l.start != nil {
		return l.start()
	}
	return nil
}

func (l *lifecycle) Stop() error {
	if l.start != nil {
		return l.stop()
	}
	return nil
}

type staticNodeConfigFetcher struct {
	config *arbnode.Config
}

func NewFetcherFromConfig(c *arbnode.Config) *staticNodeConfigFetcher {
	err := c.Validate()
	if err != nil {
		panic("invalid static config: " + err.Error())
	}
	return &staticNodeConfigFetcher{c}
}

func (c *staticNodeConfigFetcher) Get() *arbnode.Config {
	return c.config
}

func (c *staticNodeConfigFetcher) Start(context.Context) {}

func (c *staticNodeConfigFetcher) StopAndWait() {}

func (c *staticNodeConfigFetcher) Started() bool {
	return true
}

func createTestL1BlockChain(t *testing.T, l1info info) (info, *ethclient.Client, *eth.Ethereum, *node.Node) {
	return createTestL1BlockChainWithConfig(t, l1info, nil)
}

func stackConfigForTest(t *testing.T) *node.Config {
	stackConfig := node.DefaultConfig
	stackConfig.HTTPPort = 0
	stackConfig.WSPort = 0
	stackConfig.UseLightweightKDF = true
	stackConfig.P2P.ListenAddr = ""
	stackConfig.P2P.NoDial = true
	stackConfig.P2P.NoDiscovery = true
	stackConfig.P2P.NAT = nil
	stackConfig.DataDir = t.TempDir()
	return &stackConfig
}

func createDefaultStackForTest(dataDir string) (*node.Node, error) {
	stackConf := node.DefaultConfig
	var err error
	stackConf.DataDir = dataDir
	stackConf.HTTPHost = ""
	stackConf.HTTPModules = append(stackConf.HTTPModules, "eth")
	stackConf.P2P.NoDiscovery = true
	stackConf.P2P.ListenAddr = ""

	stack, err := node.New(&stackConf)
	if err != nil {
		return nil, fmt.Errorf("error creating protocol stack: %w", err)
	}
	return stack, nil
}

func createTestValidationNode(t *testing.T, ctx context.Context, config *valnode.Config) (*valnode.ValidationNode, *node.Node) {
	stackConf := node.DefaultConfig
	stackConf.HTTPPort = 0
	stackConf.DataDir = ""
	stackConf.WSHost = "127.0.0.1"
	stackConf.WSPort = 0
	stackConf.WSModules = []string{server_api.Namespace}
	stackConf.P2P.NoDiscovery = true
	stackConf.P2P.ListenAddr = ""

	valnode.EnsureValidationExposedViaAuthRPC(&stackConf)

	stack, err := node.New(&stackConf)
	Require(t, err)

	configFetcher := func() *valnode.Config { return config }
	valnode, err := valnode.CreateValidationNode(configFetcher, stack, nil)
	Require(t, err)

	err = stack.Start()
	Require(t, err)

	err = valnode.Start(ctx)
	Require(t, err)

	go func() {
		<-ctx.Done()
		stack.Close()
	}()

	return valnode, stack
}

type validated interface {
	Validate() error
}

func StaticFetcherFrom[T any](t *testing.T, config *T) func() *T {
	t.Helper()
	tCopy := *config
	asEmptyIf := interface{}(&tCopy)
	if asValidtedIf, ok := asEmptyIf.(validated); ok {
		err := asValidtedIf.Validate()
		if err != nil {
			Fatal(t, err)
		}
	}
	return func() *T { return &tCopy }
}

func configByValidationNode(t *testing.T, clientConfig *arbnode.Config, valStack *node.Node) {
	clientConfig.BlockValidator.ValidationServer.URL = valStack.WSEndpoint()
	clientConfig.BlockValidator.ValidationServer.JWTSecret = ""
}

func AddDefaultValNode(t *testing.T, ctx context.Context, nodeConfig *arbnode.Config, useJit bool) {
	if !nodeConfig.ValidatorRequired() {
		return
	}
	if nodeConfig.BlockValidator.ValidationServer.URL != "" {
		return
	}
	conf := valnode.TestValidationConfig
	conf.UseJit = useJit
	_, valStack := createTestValidationNode(t, ctx, &conf)
	configByValidationNode(t, nodeConfig, valStack)
}

func createTestL1BlockChainWithConfig(t *testing.T, l1info info, stackConfig *node.Config) (info, *ethclient.Client, *eth.Ethereum, *node.Node) {
	if l1info == nil {
		l1info = NewL1TestInfo(t)
	}
	if stackConfig == nil {
		stackConfig = stackConfigForTest(t)
	}
	l1info.GenerateAccount("Faucet")

	chainConfig := params.ArbitrumDevTestChainConfig()
	chainConfig.ArbitrumChainParams = params.ArbitrumChainParams{}

	stack, err := node.New(stackConfig)
	Require(t, err)

	nodeConf := ethconfig.Defaults
	nodeConf.NetworkId = chainConfig.ChainID.Uint64()
	l1Genesis := core.DeveloperGenesisBlock(0, 15_000_000, l1info.GetAddress("Faucet"))
	infoGenesis := l1info.GetGenesisAlloc()
	for acct, info := range infoGenesis {
		l1Genesis.Alloc[acct] = info
	}
	l1Genesis.BaseFee = big.NewInt(50 * params.GWei)
	nodeConf.Genesis = l1Genesis
	nodeConf.Miner.Etherbase = l1info.GetAddress("Faucet")

	l1backend, err := eth.New(stack, &nodeConf)
	Require(t, err)
	tempKeyStore := keystore.NewPlaintextKeyStore(t.TempDir())
	faucetAccount, err := tempKeyStore.ImportECDSA(l1info.Accounts["Faucet"].PrivateKey, "passphrase")
	Require(t, err)
	Require(t, tempKeyStore.Unlock(faucetAccount, "passphrase"))
	l1backend.AccountManager().AddBackend(tempKeyStore)
	l1backend.SetEtherbase(l1info.GetAddress("Faucet"))

	stack.RegisterLifecycle(&lifecycle{stop: func() error {
		l1backend.StopMining()
		return nil
	}})

	stack.RegisterAPIs([]rpc.API{{
		Namespace: "eth",
		Service:   filters.NewFilterAPI(filters.NewFilterSystem(l1backend.APIBackend, filters.Config{}), false),
	}})

	Require(t, stack.Start())
	Require(t, l1backend.StartMining())

	rpcClient, err := stack.Attach()
	Require(t, err)

	l1Client := ethclient.NewClient(rpcClient)

	return l1info, l1Client, l1backend, stack
}

func getInitMessage(ctx context.Context, t *testing.T, l1client client, addresses *chaininfo.RollupAddresses) *arbostypes.ParsedInitMessage {
	bridge, err := arbnode.NewDelayedBridge(l1client, addresses.Bridge, addresses.DeployedAt)
	Require(t, err)
	deployedAtBig := arbmath.UintToBig(addresses.DeployedAt)
	messages, err := bridge.LookupMessagesInRange(ctx, deployedAtBig, deployedAtBig, nil)
	Require(t, err)
	if len(messages) == 0 {
		Fatal(t, "No delayed messages found at rollup creation block")
	}
	initMessage, err := messages[0].Message.ParseInitMessage()
	Require(t, err, "Failed to parse rollup init message")

	return initMessage
}

func DeployOnTestL1(
	t *testing.T, ctx context.Context, l1info info, l1client client, chainConfig *params.ChainConfig,
) (*chaininfo.RollupAddresses, *arbostypes.ParsedInitMessage) {
	l1info.GenerateAccount("RollupOwner")
	l1info.GenerateAccount("Sequencer")
	l1info.GenerateAccount("User")

	SendWaitTestTransactions(t, ctx, l1client, []*types.Transaction{
		l1info.PrepareTx("Faucet", "RollupOwner", 30000, big.NewInt(9223372036854775807), nil),
		l1info.PrepareTx("Faucet", "Sequencer", 30000, big.NewInt(9223372036854775807), nil),
		l1info.PrepareTx("Faucet", "User", 30000, big.NewInt(9223372036854775807), nil)})

	l1TransactionOpts := l1info.GetDefaultTransactOpts("RollupOwner", ctx)
	locator, err := server_common.NewMachineLocator("")
	Require(t, err)
	serializedChainConfig, err := json.Marshal(chainConfig)
	Require(t, err)

	arbSys, _ := precompilesgen.NewArbSys(types.ArbSysAddress, l1client)
	l1Reader, err := headerreader.New(ctx, l1client, func() *headerreader.Config { return &headerreader.TestConfig }, arbSys)
	Require(t, err)
	l1Reader.Start(ctx)
	defer l1Reader.StopAndWait()

	addresses, err := arbnode.DeployOnL1(
		ctx,
		l1Reader,
		&l1TransactionOpts,
		l1info.GetAddress("Sequencer"),
		0,
		arbnode.GenerateRollupConfig(false, locator.LatestWasmModuleRoot(), l1info.GetAddress("RollupOwner"), chainConfig, serializedChainConfig, common.Address{}),
	)
	Require(t, err)
	l1info.SetContract("Bridge", addresses.Bridge)
	l1info.SetContract("SequencerInbox", addresses.SequencerInbox)
	l1info.SetContract("Inbox", addresses.Inbox)
	initMessage := getInitMessage(ctx, t, l1client, addresses)
	return addresses, initMessage
}

func createL2BlockChain(
	t *testing.T, l2info *BlockchainTestInfo, dataDir string, chainConfig *params.ChainConfig, cacheConfig *core.CacheConfig,
) (*BlockchainTestInfo, *node.Node, ethdb.Database, ethdb.Database, *core.BlockChain) {
	return createL2BlockChainWithStackConfig(t, l2info, dataDir, chainConfig, nil, nil, cacheConfig)
}

func createL2BlockChainWithStackConfig(
	t *testing.T, l2info *BlockchainTestInfo, dataDir string, chainConfig *params.ChainConfig, initMessage *arbostypes.ParsedInitMessage, stackConfig *node.Config, cacheConfig *core.CacheConfig,
) (*BlockchainTestInfo, *node.Node, ethdb.Database, ethdb.Database, *core.BlockChain) {
	if l2info == nil {
		l2info = NewArbTestInfo(t, chainConfig.ChainID)
	}
	var stack *node.Node
	var err error
	if stackConfig == nil {
		stack, err = createDefaultStackForTest(dataDir)
		Require(t, err)
	} else {
		stack, err = node.New(stackConfig)
		Require(t, err)
	}

	chainDb, err := stack.OpenDatabase("chaindb", 0, 0, "", false)
	Require(t, err)
	arbDb, err := stack.OpenDatabase("arbdb", 0, 0, "", false)
	Require(t, err)

	initReader := statetransfer.NewMemoryInitDataReader(&l2info.ArbInitData)
	if initMessage == nil {
		serializedChainConfig, err := json.Marshal(chainConfig)
		Require(t, err)
		initMessage = &arbostypes.ParsedInitMessage{
			ChainId:               chainConfig.ChainID,
			InitialL1BaseFee:      arbostypes.DefaultInitialL1BaseFee,
			ChainConfig:           chainConfig,
			SerializedChainConfig: serializedChainConfig,
		}
	}
	blockchain, err := execution.WriteOrTestBlockChain(chainDb, cacheConfig, initReader, chainConfig, initMessage, arbnode.ConfigDefaultL2Test().TxLookupLimit, 0)
	Require(t, err)

	return l2info, stack, chainDb, arbDb, blockchain
}

func ClientForStack(t *testing.T, backend *node.Node) *ethclient.Client {
	rpcClient, err := backend.Attach()
	Require(t, err)
	return ethclient.NewClient(rpcClient)
}

// Create and deploy L1 and arbnode for L2
func createTestNodeOnL1(
	t *testing.T,
	ctx context.Context,
	isSequencer bool,
) (
	l2info info, node *arbnode.Node, l2client *ethclient.Client, l1info info,
	l1backend *eth.Ethereum, l1client *ethclient.Client, l1stack *node.Node,
) {
	return createTestNodeOnL1WithConfig(t, ctx, isSequencer, nil, nil, nil)
}

func createTestNodeOnL1WithConfig(
	t *testing.T,
	ctx context.Context,
	isSequencer bool,
	nodeConfig *arbnode.Config,
	chainConfig *params.ChainConfig,
	stackConfig *node.Config,
) (
	l2info info, currentNode *arbnode.Node, l2client *ethclient.Client, l1info info,
	l1backend *eth.Ethereum, l1client *ethclient.Client, l1stack *node.Node,
) {
	l2info, currentNode, l2client, _, l1info, l1backend, l1client, l1stack = createTestNodeOnL1WithConfigImpl(t, ctx, isSequencer, nodeConfig, chainConfig, stackConfig, nil, nil)
	return
}

func createTestNodeOnL1WithConfigImpl(
	t *testing.T,
	ctx context.Context,
	isSequencer bool,
	nodeConfig *arbnode.Config,
	chainConfig *params.ChainConfig,
	stackConfig *node.Config,
	cacheConfig *core.CacheConfig,
	l2info_in info,
) (
	l2info info, currentNode *arbnode.Node, l2client *ethclient.Client, l2stack *node.Node,
	l1info info, l1backend *eth.Ethereum, l1client *ethclient.Client, l1stack *node.Node,
) {
	if nodeConfig == nil {
		nodeConfig = arbnode.ConfigDefaultL1Test()
	}
	if chainConfig == nil {
		chainConfig = params.ArbitrumDevTestChainConfig()
	}
	fatalErrChan := make(chan error, 10)
	l1info, l1client, l1backend, l1stack = createTestL1BlockChain(t, nil)
	var l2chainDb ethdb.Database
	var l2arbDb ethdb.Database
	var l2blockchain *core.BlockChain
	l2info = l2info_in
	if l2info == nil {
		l2info = NewArbTestInfo(t, chainConfig.ChainID)
	}
	addresses, initMessage := DeployOnTestL1(t, ctx, l1info, l1client, chainConfig)
	_, l2stack, l2chainDb, l2arbDb, l2blockchain = createL2BlockChainWithStackConfig(t, l2info, "", chainConfig, initMessage, stackConfig, cacheConfig)
	var sequencerTxOptsPtr *bind.TransactOpts
	var dataSigner signature.DataSignerFunc
	if isSequencer {
		sequencerTxOpts := l1info.GetDefaultTransactOpts("Sequencer", ctx)
		sequencerTxOptsPtr = &sequencerTxOpts
		dataSigner = signature.DataSignerFromPrivateKey(l1info.GetInfoWithPrivKey("Sequencer").PrivateKey)
	}

	if !isSequencer {
		nodeConfig.BatchPoster.Enable = false
		nodeConfig.Sequencer.Enable = false
		nodeConfig.DelayedSequencer.Enable = false
	}

	AddDefaultValNode(t, ctx, nodeConfig, true)

	var err error
	currentNode, err = arbnode.CreateNode(
		ctx, l2stack, l2chainDb, l2arbDb, NewFetcherFromConfig(nodeConfig), l2blockchain, l1client,
		addresses, sequencerTxOptsPtr, sequencerTxOptsPtr, dataSigner, fatalErrChan,
	)
	Require(t, err)

	Require(t, currentNode.Start(ctx))

	l2client = ClientForStack(t, l2stack)

	StartWatchChanErr(t, ctx, fatalErrChan, currentNode)

	return
}

// L2 -Only. Enough for tests that needs no interface to L1
// Requires precompiles.AllowDebugPrecompiles = true
func CreateTestL2(t *testing.T, ctx context.Context) (*BlockchainTestInfo, *arbnode.Node, *ethclient.Client) {
	return CreateTestL2WithConfig(t, ctx, nil, arbnode.ConfigDefaultL2Test(), true)
}

func CreateTestL2WithConfig(
	t *testing.T, ctx context.Context, l2Info *BlockchainTestInfo, nodeConfig *arbnode.Config, takeOwnership bool,
) (*BlockchainTestInfo, *arbnode.Node, *ethclient.Client) {
	feedErrChan := make(chan error, 10)

	AddDefaultValNode(t, ctx, nodeConfig, true)

	l2info, stack, chainDb, arbDb, blockchain := createL2BlockChain(t, l2Info, "", params.ArbitrumDevTestChainConfig(), nil)
	currentNode, err := arbnode.CreateNode(ctx, stack, chainDb, arbDb, NewFetcherFromConfig(nodeConfig), blockchain, nil, nil, nil, nil, nil, feedErrChan)
	Require(t, err)

	// Give the node an init message
	err = currentNode.TxStreamer.AddFakeInitMessage()
	Require(t, err)

	Require(t, currentNode.Start(ctx))
	client := ClientForStack(t, stack)

	if takeOwnership {
		debugAuth := l2info.GetDefaultTransactOpts("Owner", ctx)

		// make auth a chain owner
		arbdebug, err := precompilesgen.NewArbDebug(common.HexToAddress("0xff"), client)
		Require(t, err, "failed to deploy ArbDebug")

		tx, err := arbdebug.BecomeChainOwner(&debugAuth)
		Require(t, err, "failed to deploy ArbDebug")

		_, err = EnsureTxSucceeded(ctx, client, tx)
		Require(t, err)
	}

	StartWatchChanErr(t, ctx, feedErrChan, currentNode)

	return l2info, currentNode, client
}

func StartWatchChanErr(t *testing.T, ctx context.Context, feedErrChan chan error, node *arbnode.Node) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case err := <-feedErrChan:
			t.Errorf("error occurred: %v", err)
			if node != nil {
				node.StopAndWait()
			}
		}
	}()
}

func Require(t *testing.T, err error, text ...interface{}) {
	t.Helper()
	testhelpers.RequireImpl(t, err, text...)
}

func Fatal(t *testing.T, printables ...interface{}) {
	t.Helper()
	testhelpers.FailImpl(t, printables...)
}

func Create2ndNode(
	t *testing.T,
	ctx context.Context,
	first *arbnode.Node,
	l1stack *node.Node,
	l1info *BlockchainTestInfo,
	l2InitData *statetransfer.ArbosInitializationInfo,
	dasConfig *das.DataAvailabilityConfig,
) (*ethclient.Client, *arbnode.Node) {
	nodeConf := arbnode.ConfigDefaultL1NonSequencerTest()
	if dasConfig == nil {
		nodeConf.DataAvailability.Enable = false
	} else {
		nodeConf.DataAvailability = *dasConfig
	}
	return Create2ndNodeWithConfig(t, ctx, first, l1stack, l1info, l2InitData, nodeConf, nil)
}

func Create2ndNodeWithConfig(
	t *testing.T,
	ctx context.Context,
	first *arbnode.Node,
	l1stack *node.Node,
	l1info *BlockchainTestInfo,
	l2InitData *statetransfer.ArbosInitializationInfo,
	nodeConfig *arbnode.Config,
	stackConfig *node.Config,
) (*ethclient.Client, *arbnode.Node) {
	feedErrChan := make(chan error, 10)
	l1rpcClient, err := l1stack.Attach()
	if err != nil {
		Fatal(t, err)
	}
	l1client := ethclient.NewClient(l1rpcClient)

	if stackConfig == nil {
		stackConfig = stackConfigForTest(t)
	}
	l2stack, err := node.New(stackConfig)
	Require(t, err)

	l2chainDb, err := l2stack.OpenDatabase("chaindb", 0, 0, "", false)
	Require(t, err)
	l2arbDb, err := l2stack.OpenDatabase("arbdb", 0, 0, "", false)
	Require(t, err)
	initReader := statetransfer.NewMemoryInitDataReader(l2InitData)

	dataSigner := signature.DataSignerFromPrivateKey(l1info.GetInfoWithPrivKey("Sequencer").PrivateKey)
	txOpts := l1info.GetDefaultTransactOpts("Sequencer", ctx)
	chainConfig := first.Execution.ArbInterface.BlockChain().Config()
	initMessage := getInitMessage(ctx, t, l1client, first.DeployInfo)
	l2blockchain, err := execution.WriteOrTestBlockChain(l2chainDb, nil, initReader, chainConfig, initMessage, arbnode.ConfigDefaultL2Test().TxLookupLimit, 0)
	Require(t, err)

	AddDefaultValNode(t, ctx, nodeConfig, true)

	currentNode, err := arbnode.CreateNode(ctx, l2stack, l2chainDb, l2arbDb, NewFetcherFromConfig(nodeConfig), l2blockchain, l1client, first.DeployInfo, &txOpts, &txOpts, dataSigner, feedErrChan)
	Require(t, err)

	err = currentNode.Start(ctx)
	Require(t, err)
	l2client := ClientForStack(t, l2stack)

	StartWatchChanErr(t, ctx, feedErrChan, currentNode)

	return l2client, currentNode
}

func GetBalance(t *testing.T, ctx context.Context, client *ethclient.Client, account common.Address) *big.Int {
	t.Helper()
	balance, err := client.BalanceAt(ctx, account, nil)
	Require(t, err, "could not get balance")
	return balance
}

func requireClose(t *testing.T, s *node.Node, text ...interface{}) {
	t.Helper()
	Require(t, s.Close(), text...)
}

func authorizeDASKeyset(
	t *testing.T,
	ctx context.Context,
	dasSignerKey *blsSignatures.PublicKey,
	l1info info,
	l1client arbutil.L1Interface,
) {
	if dasSignerKey == nil {
		return
	}
	keyset := &arbstate.DataAvailabilityKeyset{
		AssumedHonest: 1,
		PubKeys:       []blsSignatures.PublicKey{*dasSignerKey},
	}
	wr := bytes.NewBuffer([]byte{})
	err := keyset.Serialize(wr)
	Require(t, err, "unable to serialize DAS keyset")
	keysetBytes := wr.Bytes()
	sequencerInbox, err := bridgegen.NewSequencerInbox(l1info.Accounts["SequencerInbox"].Address, l1client)
	Require(t, err, "unable to create sequencer inbox")
	trOps := l1info.GetDefaultTransactOpts("RollupOwner", ctx)
	tx, err := sequencerInbox.SetValidKeyset(&trOps, keysetBytes)
	Require(t, err, "unable to set valid keyset")
	_, err = EnsureTxSucceeded(ctx, l1client, tx)
	Require(t, err, "unable to ensure transaction success for setting valid keyset")
}

func setupConfigWithDAS(
	t *testing.T, ctx context.Context, dasModeString string,
) (*params.ChainConfig, *arbnode.Config, *das.LifecycleManager, string, *blsSignatures.PublicKey) {
	l1NodeConfigA := arbnode.ConfigDefaultL1Test()
	chainConfig := params.ArbitrumDevTestChainConfig()
	var dbPath string
	var err error

	enableFileStorage, enableDbStorage, enableDas := false, false, true
	switch dasModeString {
	case "db":
		enableDbStorage = true
		chainConfig = params.ArbitrumDevTestDASChainConfig()
	case "files":
		enableFileStorage = true
		chainConfig = params.ArbitrumDevTestDASChainConfig()
	case "onchain":
		enableDas = false
	default:
		Fatal(t, "unknown storage type")
	}
	dbPath = t.TempDir()
	dasSignerKey, _, err := das.GenerateAndStoreKeys(dbPath)
	Require(t, err)

	dasConfig := &das.DataAvailabilityConfig{
		Enable: enableDas,
		Key: das.KeyConfig{
			KeyDir: dbPath,
		},
		LocalFileStorage: das.LocalFileStorageConfig{
			Enable:  enableFileStorage,
			DataDir: dbPath,
		},
		LocalDBStorage: das.LocalDBStorageConfig{
			Enable:  enableDbStorage,
			DataDir: dbPath,
		},
		RequestTimeout:           5 * time.Second,
		ParentChainNodeURL:       "none",
		SequencerInboxAddress:    "none",
		PanicOnError:             true,
		DisableSignatureChecking: true,
	}

	l1NodeConfigA.DataAvailability = das.DefaultDataAvailabilityConfig
	var lifecycleManager *das.LifecycleManager
	var daReader das.DataAvailabilityServiceReader
	var daWriter das.DataAvailabilityServiceWriter
	var daHealthChecker das.DataAvailabilityServiceHealthChecker
	if dasModeString != "onchain" {
		daReader, daWriter, daHealthChecker, lifecycleManager, err = das.CreateDAComponentsForDaserver(ctx, dasConfig, nil, nil)

		Require(t, err)
		rpcLis, err := net.Listen("tcp", "localhost:0")
		Require(t, err)
		restLis, err := net.Listen("tcp", "localhost:0")
		Require(t, err)
		_, err = das.StartDASRPCServerOnListener(ctx, rpcLis, genericconf.HTTPServerTimeoutConfigDefault, daReader, daWriter, daHealthChecker)
		Require(t, err)
		_, err = das.NewRestfulDasServerOnListener(restLis, genericconf.HTTPServerTimeoutConfigDefault, daReader, daHealthChecker)
		Require(t, err)

		beConfigA := das.BackendConfig{
			URL:                 "http://" + rpcLis.Addr().String(),
			PubKeyBase64Encoded: blsPubToBase64(dasSignerKey),
			SignerMask:          1,
		}
		l1NodeConfigA.DataAvailability.RPCAggregator = aggConfigForBackend(t, beConfigA)
		l1NodeConfigA.DataAvailability.Enable = true
		l1NodeConfigA.DataAvailability.RestAggregator = das.DefaultRestfulClientAggregatorConfig
		l1NodeConfigA.DataAvailability.RestAggregator.Enable = true
		l1NodeConfigA.DataAvailability.RestAggregator.Urls = []string{"http://" + restLis.Addr().String()}
		l1NodeConfigA.DataAvailability.ParentChainNodeURL = "none"
	}

	return chainConfig, l1NodeConfigA, lifecycleManager, dbPath, dasSignerKey
}

func getDeadlineTimeout(t *testing.T, defaultTimeout time.Duration) time.Duration {
	testDeadLine, deadlineExist := t.Deadline()
	var timeout time.Duration
	if deadlineExist {
		timeout = time.Until(testDeadLine) - (time.Second * 10)
		if timeout > time.Second*10 {
			timeout = timeout - (time.Second * 10)
		}
	} else {
		timeout = defaultTimeout
	}

	return timeout
}

func deploySimple(
	t *testing.T, ctx context.Context, auth bind.TransactOpts, client *ethclient.Client,
) (common.Address, *mocksgen.Simple) {
	addr, tx, simple, err := mocksgen.DeploySimple(&auth, client)
	Require(t, err, "could not deploy Simple.sol contract")
	_, err = EnsureTxSucceeded(ctx, client, tx)
	Require(t, err)
	return addr, simple
}

func TestMain(m *testing.M) {
	logLevelEnv := os.Getenv("TEST_LOGLEVEL")
	if logLevelEnv != "" {
		logLevel, err := strconv.ParseUint(logLevelEnv, 10, 32)
		if err != nil || logLevel > uint64(log.LvlTrace) {
			log.Warn("TEST_LOGLEVEL exists but out of bound, ignoring", "logLevel", logLevelEnv, "max", log.LvlTrace)
		}
		glogger := log.NewGlogHandler(log.StreamHandler(os.Stderr, log.TerminalFormat(false)))
		glogger.Verbosity(log.Lvl(logLevel))
		log.Root().SetHandler(glogger)
	}
	code := m.Run()
	os.Exit(code)
}
