package indexer

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cyberhorsey/errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/taikoxyz/taiko-mono/packages/eventindexer"
	"github.com/taikoxyz/taiko-mono/packages/eventindexer/contracts/assignmenthook"
	"github.com/taikoxyz/taiko-mono/packages/eventindexer/contracts/bridge"
	"github.com/taikoxyz/taiko-mono/packages/eventindexer/contracts/taikol1"
	"github.com/taikoxyz/taiko-mono/packages/eventindexer/pkg/repo"
	"github.com/urfave/cli/v2"
)

var (
	ZeroAddress = common.HexToAddress("0x0000000000000000000000000000000000000000")
)

var (
	Layer1 = "l1"
	Layer2 = "l2"
)

type SyncMode string

var (
	Sync   SyncMode = "sync"
	Resync SyncMode = "resync"
	Modes           = []SyncMode{Sync, Resync}
)

type Indexer struct {
	accountRepo      eventindexer.AccountRepository
	eventRepo        eventindexer.EventRepository
	nftBalanceRepo   eventindexer.NFTBalanceRepository
	erc20BalanceRepo eventindexer.ERC20BalanceRepository
	txRepo           eventindexer.TransactionRepository

	ethClient  *ethclient.Client
	srcChainID uint64

	latestIndexedBlockNumber uint64

	blockBatchSize      uint64
	subscriptionBackoff time.Duration

	taikol1        *taikol1.TaikoL1
	bridge         *bridge.Bridge
	assignmentHook *assignmenthook.AssignmentHook

	indexNfts   bool
	indexERC20s bool
	layer       string

	wg  *sync.WaitGroup
	ctx context.Context

	syncMode SyncMode

	blockSaveMutex *sync.Mutex
}

func (i *Indexer) Start() error {
	i.ctx = context.Background()

	if err := i.setInitialIndexingBlockByMode(i.ctx, i.syncMode); err != nil {
		return errors.Wrap(err, "i.setInitialIndexingBlockByMode")
	}

	i.wg.Add(1)

	go i.eventLoop(i.ctx)

	return nil
}

func (i *Indexer) eventLoop(ctx context.Context) {
	defer i.wg.Done()

	t := time.NewTicker(10 * time.Second)

	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("event loop context done")
			return
		case <-t.C:
			if err := i.filter(ctx, filterFunc); err != nil {
				slog.Error("error filtering", "error", err)
			}
		}
	}
}

func (i *Indexer) Name() string {
	return "indexer"
}

func (i *Indexer) InitFromCli(ctx context.Context, c *cli.Context) error {
	cfg, err := NewConfigFromCliContext(c)
	if err != nil {
		return err
	}

	return InitFromConfig(ctx, i, cfg)
}

// nolint: funlen
func InitFromConfig(ctx context.Context, i *Indexer, cfg *Config) error {
	db, err := cfg.OpenDBFunc()
	if err != nil {
		return err
	}

	accountRepository, err := repo.NewAccountRepository(db)
	if err != nil {
		return err
	}

	eventRepository, err := repo.NewEventRepository(db)
	if err != nil {
		return err
	}

	nftBalanceRepository, err := repo.NewNFTBalanceRepository(db)
	if err != nil {
		return err
	}

	erc20BalanceRepository, err := repo.NewERC20BalanceRepository(db)
	if err != nil {
		return err
	}

	txRepository, err := repo.NewTransactionRepository(db)
	if err != nil {
		return err
	}

	ethClient, err := ethclient.Dial(cfg.RPCUrl)
	if err != nil {
		return err
	}

	chainID, err := ethClient.ChainID(ctx)
	if err != nil {
		return errors.Wrap(err, "i.ethClient.ChainID()")
	}

	var taikoL1 *taikol1.TaikoL1

	if cfg.L1TaikoAddress.Hex() != ZeroAddress.Hex() {
		slog.Info("setting l1TaikoAddress", "addr", cfg.L1TaikoAddress.Hex())

		taikoL1, err = taikol1.NewTaikoL1(cfg.L1TaikoAddress, ethClient)
		if err != nil {
			return errors.Wrap(err, "contracts.NewTaikoL1")
		}
	}

	var bridgeContract *bridge.Bridge

	if cfg.BridgeAddress.Hex() != ZeroAddress.Hex() {
		slog.Info("setting bridgeADdress", "addr", cfg.BridgeAddress.Hex())

		bridgeContract, err = bridge.NewBridge(cfg.BridgeAddress, ethClient)
		if err != nil {
			return errors.Wrap(err, "contracts.NewBridge")
		}
	}

	var assignmentHookContract *assignmenthook.AssignmentHook

	if cfg.AssignmentHookAddress.Hex() != ZeroAddress.Hex() {
		slog.Info("setting assignmentHookAddress", "addr", cfg.AssignmentHookAddress.Hex())

		assignmentHookContract, err = assignmenthook.NewAssignmentHook(cfg.AssignmentHookAddress, ethClient)
		if err != nil {
			return errors.Wrap(err, "contracts.NewAssignmentHook")
		}
	}

	i.blockSaveMutex = &sync.Mutex{}
	i.accountRepo = accountRepository
	i.eventRepo = eventRepository
	i.nftBalanceRepo = nftBalanceRepository
	i.erc20BalanceRepo = erc20BalanceRepository
	i.txRepo = txRepository

	i.srcChainID = chainID.Uint64()

	i.ethClient = ethClient
	i.taikol1 = taikoL1
	i.bridge = bridgeContract
	i.assignmentHook = assignmentHookContract
	i.blockBatchSize = cfg.BlockBatchSize
	i.subscriptionBackoff = time.Duration(cfg.SubscriptionBackoff) * time.Second
	i.wg = &sync.WaitGroup{}

	i.syncMode = cfg.SyncMode
	i.indexNfts = cfg.IndexNFTs
	i.indexERC20s = cfg.IndexERC20s
	i.layer = cfg.Layer

	return nil
}

func (i *Indexer) Close(ctx context.Context) {
	i.wg.Wait()
}
