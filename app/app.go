package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/gov"
	"github.com/cosmos/cosmos-sdk/x/stake"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/tmhash"
	cmn "github.com/tendermint/tendermint/libs/common"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	tmtypes "github.com/tendermint/tendermint/types"

	"github.com/BiJie/BinanceChain/app/config"
	"github.com/BiJie/BinanceChain/app/pub"
	"github.com/BiJie/BinanceChain/app/val"
	"github.com/BiJie/BinanceChain/common"
	"github.com/BiJie/BinanceChain/common/fees"
	bnclog "github.com/BiJie/BinanceChain/common/log"
	"github.com/BiJie/BinanceChain/common/tx"
	"github.com/BiJie/BinanceChain/common/types"
	"github.com/BiJie/BinanceChain/common/utils"
	"github.com/BiJie/BinanceChain/plugins/dex"
	"github.com/BiJie/BinanceChain/plugins/dex/order"
	"github.com/BiJie/BinanceChain/plugins/ico"
	"github.com/BiJie/BinanceChain/plugins/param"
	"github.com/BiJie/BinanceChain/plugins/param/paramhub"
	"github.com/BiJie/BinanceChain/plugins/tokens"
	tkstore "github.com/BiJie/BinanceChain/plugins/tokens/store"
	"github.com/BiJie/BinanceChain/wire"
)

const (
	appName = "BNBChain"
)

// default home directories for expected binaries
var (
	DefaultCLIHome  = os.ExpandEnv("$HOME/.bnbcli")
	DefaultNodeHome = os.ExpandEnv("$HOME/.bnbchaind")
)

// BinanceChain implements ChainApp
var _ types.ChainApp = (*BinanceChain)(nil)

var (
	Codec         = MakeCodec()
	ServerContext = config.NewDefaultContext()
)

// BinanceChain is the BNBChain ABCI application
type BinanceChain struct {
	*baseapp.BaseApp
	Codec *wire.Codec

	// the abci query handler mapping is `prefix -> handler`
	queryHandlers map[string]types.AbciQueryHandler

	// keepers
	CoinKeeper    bank.Keeper
	DexKeeper     *dex.DexKeeper
	AccountKeeper auth.AccountKeeper
	TokenMapper   tkstore.Mapper
	ValAddrMapper val.Mapper
	stakeKeeper   stake.Keeper
	govKeeper     gov.Keeper
	// keeper to process param store and update
	ParamHub *param.ParamHub

	baseConfig        *config.BaseConfig
	publicationConfig *config.PublicationConfig
	publisher         pub.MarketDataPublisher

	// Unlike tendermint, we don't need implement a no-op metrics, usage of this field should
	// check nil-ness to know whether metrics collection is turn on
	// TODO(#246): make it an aggregated wrapper of all component metrics (i.e. DexKeeper, StakeKeeper)
	metrics *pub.Metrics
}

// NewBinanceChain creates a new instance of the BinanceChain.
func NewBinanceChain(logger log.Logger, db dbm.DB, traceStore io.Writer, baseAppOptions ...func(*baseapp.BaseApp)) *BinanceChain {

	// create app-level codec for txs and accounts
	var cdc = Codec

	// create composed tx decoder
	decoders := wire.ComposeTxDecoders(cdc, defaultTxDecoder)

	// create the applicationsimulate object
	var app = &BinanceChain{
		BaseApp:           baseapp.NewBaseApp(appName /*, cdc*/, logger, db, decoders, ServerContext.PublishAccountBalance, baseAppOptions...),
		Codec:             cdc,
		queryHandlers:     make(map[string]types.AbciQueryHandler),
		baseConfig:        ServerContext.BaseConfig,
		publicationConfig: ServerContext.PublicationConfig,
	}

	app.SetCommitMultiStoreTracer(traceStore)

	// mappers
	app.AccountKeeper = auth.NewAccountKeeper(cdc, common.AccountStoreKey, types.ProtoAppAccount)
	app.TokenMapper = tkstore.NewMapper(cdc, common.TokenStoreKey)
	app.ValAddrMapper = val.NewMapper(common.ValAddrStoreKey)
	app.CoinKeeper = bank.NewBaseKeeper(app.AccountKeeper)
	app.ParamHub = paramhub.NewKeeper(cdc, common.ParamsStoreKey, common.TParamsStoreKey)
	app.stakeKeeper = stake.NewKeeper(
		cdc,
		common.StakeStoreKey, common.TStakeStoreKey,
		app.CoinKeeper, app.ParamHub.Subspace(stake.DefaultParamspace),
		app.RegisterCodespace(stake.DefaultCodespace),
	)
	app.govKeeper = gov.NewKeeper(
		cdc,
		common.GovStoreKey,
		app.ParamHub.Keeper, app.ParamHub.Subspace(gov.DefaultParamspace), app.CoinKeeper, app.stakeKeeper,
		app.RegisterCodespace(gov.DefaultCodespace),
	)
	app.ParamHub.SetGovKeeper(app.govKeeper)
	// legacy bank route (others moved to plugin init funcs)
	app.Router().
		AddRoute("bank", bank.NewHandler(app.CoinKeeper)).
		AddRoute("stake", stake.NewHandler(app.stakeKeeper)).
		AddRoute("gov", gov.NewHandler(app.govKeeper))

	app.QueryRouter().AddRoute("gov", gov.NewQuerier(app.govKeeper))

	if ServerContext.Config.Instrumentation.Prometheus {
		app.metrics = pub.PrometheusMetrics() // TODO(#246): make it an aggregated wrapper of all component metrics (i.e. DexKeeper, StakeKeeper)
	}

	if app.publicationConfig.ShouldPublishAny() {
		app.publisher = pub.NewKafkaMarketDataPublisher(app.Logger, app.publicationConfig, app.metrics)
	}

	// finish app initialization
	app.SetInitChainer(app.initChainerFn())
	app.SetEndBlocker(app.EndBlocker)
	app.MountStoresIAVL(
		common.MainStoreKey,
		common.AccountStoreKey,
		common.ValAddrStoreKey,
		common.TokenStoreKey,
		common.DexStoreKey,
		common.PairStoreKey,
		common.ParamsStoreKey,
		common.StakeStoreKey,
		common.GovStoreKey,
	)
	app.SetAnteHandler(tx.NewAnteHandler(app.AccountKeeper))
	app.SetPreChecker(tx.NewTxPreChecker(app.AccountKeeper))
	app.MountStoresTransient(common.TParamsStoreKey, common.TStakeStoreKey)

	// block store required to hydrate dex OB
	err := app.LoadCMSLatestVersion()
	if err != nil {
		cmn.Exit(err.Error())
	}

	// init app cache
	accountStore := app.BaseApp.GetCommitMultiStore().GetKVStore(common.AccountStoreKey)
	app.SetAccountStoreCache(cdc, accountStore, app.baseConfig.AccountCacheSize)

	tx.InitSigCache(app.baseConfig.SignatureCacheSize)

	err = app.InitFromStore(common.MainStoreKey)
	if err != nil {
		cmn.Exit(err.Error())
	}

	// remaining plugin init
	app.initDex()
	app.initPlugins()
	app.initParams()
	return app
}

func (app *BinanceChain) initDex() {
	tradingPairMapper := dex.NewTradingPairMapper(app.Codec, common.PairStoreKey)
	// TODO: make the concurrency configurable
	app.DexKeeper = dex.NewOrderKeeper(common.DexStoreKey, app.AccountKeeper, tradingPairMapper,
		app.RegisterCodespace(dex.DefaultCodespace), 2, app.Codec, app.publicationConfig.ShouldPublishAny())
	app.DexKeeper.SubscribeParamChange(app.ParamHub)
	// do not proceed if we are in a unit test and `CheckState` is unset.
	if app.CheckState == nil {
		return
	}
	// count back to 7 days.
	app.DexKeeper.InitOrderBook(app.CheckState.Ctx, 7,
		baseapp.LoadBlockDB(), app.LastBlockHeight(), app.TxDecoder)
}

func (app *BinanceChain) initPlugins() {
	tokens.InitPlugin(app, app.TokenMapper, app.AccountKeeper, app.CoinKeeper)
	dex.InitPlugin(app, app.DexKeeper, app.TokenMapper, app.AccountKeeper, app.govKeeper)
	param.InitPlugin(app, app.ParamHub)
}

func (app *BinanceChain) initParams() {
	if app.CheckState == nil || app.CheckState.Ctx.BlockHeight() == 0 {
		return
	}
	app.ParamHub.Load(app.CheckState.Ctx)
}

// initChainerFn performs custom logic for chain initialization.
func (app *BinanceChain) initChainerFn() sdk.InitChainer {
	return func(ctx sdk.Context, req abci.RequestInitChain) abci.ResponseInitChain {
		stateJSON := req.AppStateBytes

		genesisState := new(GenesisState)
		err := app.Codec.UnmarshalJSON(stateJSON, genesisState)
		if err != nil {
			panic(err) // TODO https://github.com/cosmos/cosmos-sdk/issues/468
			// return sdk.ErrGenesisParse("").TraceCause(err, "")
		}

		validatorAddrs := make([]sdk.AccAddress, len(genesisState.Accounts))
		for i, gacc := range genesisState.Accounts {
			acc := gacc.ToAppAccount()
			acc.AccountNumber = app.AccountKeeper.GetNextAccountNumber(ctx)
			app.AccountKeeper.SetAccount(ctx, acc)
			app.ValAddrMapper.SetVal(ctx, gacc.ValAddr, gacc.Address)
			validatorAddrs[i] = acc.Address
		}
		tokens.InitGenesis(ctx, app.TokenMapper, app.CoinKeeper, genesisState.Tokens,
			validatorAddrs, DefaultSelfDelegationToken.Amount)

		app.ParamHub.InitGenesis(ctx, genesisState.ParamGenesis)
		validators, err := stake.InitGenesis(ctx, app.stakeKeeper, genesisState.StakeData)
		gov.InitGenesis(ctx, app.govKeeper, genesisState.GovData)

		if err != nil {
			panic(err) // TODO find a way to do this w/o panics
		}

		// before we deliver the genTxs, we have transferred some delegation tokens to the validator candidates.
		if len(genesisState.GenTxs) > 0 {
			for _, genTx := range genesisState.GenTxs {
				var tx auth.StdTx
				err = app.Codec.UnmarshalJSON(genTx, &tx)
				if err != nil {
					panic(err)
				}
				bz := app.Codec.MustMarshalBinary(tx)
				res := app.BaseApp.DeliverTx(bz)
				if !res.IsOK() {
					panic(res.Log)
				}
			}
			validators = app.stakeKeeper.ApplyAndReturnValidatorSetUpdates(ctx)
		}

		// sanity check
		if len(req.Validators) > 0 {
			if len(req.Validators) != len(validators) {
				panic(fmt.Errorf("genesis validator numbers are not matched, staked=%d, req=%d",
					len(validators), len(req.Validators)))
			}
			sort.Sort(abci.ValidatorUpdates(req.Validators))
			sort.Sort(abci.ValidatorUpdates(validators))
			for i, val := range validators {
				if !val.Equal(req.Validators[i]) {
					panic(fmt.Errorf("invalid genesis validator, index=%d", i))
				}
			}
		}

		return abci.ResponseInitChain{
			Validators: validators,
		}
	}
}

// Implements ABCI
func (app *BinanceChain) DeliverTx(txBytes []byte) (res abci.ResponseDeliverTx) {
	res = app.BaseApp.DeliverTx(txBytes)
	if res.IsOK() {
		// commit or panic
		txHash := cmn.HexBytes(tmhash.Sum(txBytes)).String()
		fees.Pool.CommitFee(txHash)
	}

	return res
}

func (app *BinanceChain) EndBlocker(ctx sdk.Context, req abci.RequestEndBlock) abci.ResponseEndBlock {
	// lastBlockTime would be 0 if this is the first block.
	lastBlockTime := app.CheckState.Ctx.BlockHeader().Time
	blockTime := ctx.BlockHeader().Time
	height := ctx.BlockHeader().Height

	var tradesToPublish []*pub.Trade

	isBreatheBlock := !utils.SameDayInUTC(lastBlockTime, blockTime)
	if height%1000 != 0 {
		// only match in the normal block
		app.Logger.Debug("normal block", "height", height)
		if app.publicationConfig.ShouldPublishAny() && pub.IsLive {
			tradesToPublish = pub.MatchAndAllocateAllForPublish(app.DexKeeper, ctx)
		} else {
			app.DexKeeper.MatchAndAllocateAll(ctx, nil)
		}
	} else {
		// breathe block
		bnclog.Info("Start Breathe Block Handling",
			"height", height, "lastBlockTime", lastBlockTime, "newBlockTime", blockTime)
		icoDone := ico.EndBlockAsync(ctx)
		dex.EndBreatheBlock(ctx, app.DexKeeper, height, blockTime)
		param.EndBreatheBlock(ctx, app.ParamHub)
		// other end blockers
		<-icoDone
	}

	blockFee := distributeFee(ctx, app.AccountKeeper, app.ValAddrMapper, app.publicationConfig.PublishBlockFee)

	if app.publicationConfig.ShouldPublishAny() &&
		pub.IsLive {
		if height >= app.publicationConfig.FromHeightInclusive {
			app.publish(tradesToPublish, blockFee, ctx, height, blockTime.Unix())
		}

		// clean up intermediate cached data
		app.DexKeeper.ClearOrderChanges()
		app.DexKeeper.ClearRoundFee()
	}

	tags := gov.EndBlocker(ctx, app.govKeeper)

	var validatorUpdates abci.ValidatorUpdates
	if isBreatheBlock {
		// some endblockers without fees will execute after publish to make publication run as early as possible.
		validatorUpdates = stake.EndBlocker(ctx, app.stakeKeeper)
	}

	//match may end with transaction failure, which is better to save into
	//the EndBlock response. However, current cosmos doesn't support this.
	//future TODO: add failure info.
	return abci.ResponseEndBlock{
		ValidatorUpdates: validatorUpdates,
		Tags:             tags,
	}
}

// ExportAppStateAndValidators exports blockchain world state to json.
func (app *BinanceChain) ExportAppStateAndValidators() (appState json.RawMessage, validators []tmtypes.GenesisValidator, err error) {
	ctx := app.NewContext(sdk.RunTxModeCheck, abci.Header{})

	// iterate to get the accounts
	accounts := []GenesisAccount{}
	appendAccount := func(acc sdk.Account) (stop bool) {
		account := GenesisAccount{
			Address: acc.GetAddress(),
		}
		accounts = append(accounts, account)
		return false
	}
	app.AccountKeeper.IterateAccounts(ctx, appendAccount)

	genState := GenesisState{
		Accounts: accounts,
	}
	appState, err = wire.MarshalJSONIndent(app.Codec, genState)
	if err != nil {
		return nil, nil, err
	}
	return appState, validators, nil
}

// Query performs an abci query.
func (app *BinanceChain) Query(req abci.RequestQuery) (res abci.ResponseQuery) {
	path := baseapp.SplitPath(req.Path)
	if len(path) == 0 {
		msg := "no query path provided"
		return sdk.ErrUnknownRequest(msg).QueryResult()
	}
	prefix := path[0]
	if handler, ok := app.queryHandlers[prefix]; ok {
		res := handler(app, req, path)
		if res == nil {
			return app.BaseApp.Query(req)
		}
		return *res
	}
	return app.BaseApp.Query(req)
}

// RegisterQueryHandler registers an abci query handler, implements ChainApp.RegisterQueryHandler.
func (app *BinanceChain) RegisterQueryHandler(prefix string, handler types.AbciQueryHandler) {
	if _, ok := app.queryHandlers[prefix]; ok {
		panic(fmt.Errorf("registerQueryHandler: prefix `%s` is already registered", prefix))
	} else {
		app.queryHandlers[prefix] = handler
	}
}

// GetCodec returns the app's Codec.
func (app *BinanceChain) GetCodec() *wire.Codec {
	return app.Codec
}

// GetRouter returns the app's Router.
func (app *BinanceChain) GetRouter() baseapp.Router {
	return app.Router()
}

// GetContextForCheckState gets the context for the check state.
func (app *BinanceChain) GetContextForCheckState() sdk.Context {
	return app.CheckState.Ctx
}

// default custom logic for transaction decoding
func defaultTxDecoder(cdc *wire.Codec) sdk.TxDecoder {
	return func(txBytes []byte) (sdk.Tx, sdk.Error) {
		var tx = auth.StdTx{}

		if len(txBytes) == 0 {
			return nil, sdk.ErrTxDecode("txBytes are empty")
		}

		// StdTx.Msg is an interface. The concrete types
		// are registered by MakeTxCodec
		err := cdc.UnmarshalBinary(txBytes, &tx)
		if err != nil {
			return nil, sdk.ErrTxDecode("").TraceSDK(err.Error())
		}
		return tx, nil
	}
}

// MakeCodec creates a custom tx codec.
func MakeCodec() *wire.Codec {
	var cdc = wire.NewCodec()

	wire.RegisterCrypto(cdc) // Register crypto.
	bank.RegisterCodec(cdc)
	sdk.RegisterCodec(cdc) // Register Msgs
	dex.RegisterWire(cdc)
	tokens.RegisterWire(cdc)
	types.RegisterWire(cdc)
	tx.RegisterWire(cdc)
	stake.RegisterCodec(cdc)
	gov.RegisterCodec(cdc)
	param.RegisterWire(cdc)
	return cdc
}

func (app *BinanceChain) publish(tradesToPublish []*pub.Trade, blockFee pub.BlockFee, ctx sdk.Context, height, blockTime int64) {
	pub.Logger.Info("start to collect publish information", "height", height)

	var accountsToPublish map[string]pub.Account
	var latestPriceLevels order.ChangedPriceLevelsMap

	duration := pub.Timer(app.Logger, fmt.Sprintf("collect publish information, height=%d", height), func() {
		if app.publicationConfig.PublishAccountBalance {
			txRelatedAccounts := app.Pool.TxRelatedAddrs()
			tradeRelatedAccounts := pub.GetTradeAndOrdersRelatedAccounts(app.DexKeeper, tradesToPublish)
			accountsToPublish = pub.GetAccountBalances(
				app.AccountKeeper,
				ctx,
				txRelatedAccounts,
				tradeRelatedAccounts,
				blockFee.Validators)
		}

		if app.publicationConfig.PublishOrderBook {
			latestPriceLevels = app.DexKeeper.GetOrderBooks(pub.MaxOrderBookLevel)
		}
	})

	if app.metrics != nil {
		app.metrics.CollectBlockTimeMs.Set(float64(duration))
	}

	pub.Logger.Info("start to publish", "height", height,
		"blockTime", blockTime, "numOfTrades", len(tradesToPublish),
		"numOfOrders", // the order num we collected here doesn't include trade related orders
		len(app.DexKeeper.OrderChanges),
		"numOfAccounts",
		len(accountsToPublish))
	pub.ToRemoveOrderIdCh = make(chan string, pub.ToRemoveOrderIdChannelSize)
	pub.ToPublishCh <- pub.NewBlockInfoToPublish(
		height,
		blockTime,
		tradesToPublish,
		app.DexKeeper.OrderChanges,    // thread-safety is guarded by the signal from RemoveDoneCh
		app.DexKeeper.OrderChangesMap, // thread-safety is guarded by the signal from RemoveDoneCh
		accountsToPublish,
		latestPriceLevels,
		blockFee,
		app.DexKeeper.RoundOrderFees)

	// remove item from OrderInfoForPublish when we published removed order (cancel, iocnofill, fullyfilled, expired)
	for id := range pub.ToRemoveOrderIdCh {
		pub.Logger.Debug("delete order from order changes map", "orderId", id)
		delete(app.DexKeeper.OrderChangesMap, id)
	}

	pub.Logger.Debug("finish publish", "height", height)
}
