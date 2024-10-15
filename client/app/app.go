package app

import (
	"io"

	"cosmossdk.io/depinject"
	"cosmossdk.io/log"
	storetypes "cosmossdk.io/store/types"
	upgradekeeper "cosmossdk.io/x/upgrade/keeper"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/runtime"
	servertypes "github.com/cosmos/cosmos-sdk/server/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/version"
	authkeeper "github.com/cosmos/cosmos-sdk/x/auth/keeper"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	distrkeeper "github.com/cosmos/cosmos-sdk/x/distribution/keeper"
	paramstypes "github.com/cosmos/cosmos-sdk/x/params/types"
	slashingkeeper "github.com/cosmos/cosmos-sdk/x/slashing/keeper"
	stakingkeeper "github.com/cosmos/cosmos-sdk/x/staking/keeper"

	"github.com/piplabs/story/client/app/keepers"
	"github.com/piplabs/story/client/app/module"
	"github.com/piplabs/story/client/comet"
	appv1 "github.com/piplabs/story/client/pkg/appconsts/v1"
	evmstakingkeeper "github.com/piplabs/story/client/x/evmstaking/keeper"
	mintkeeper "github.com/piplabs/story/client/x/mint/keeper"
	"github.com/piplabs/story/lib/errors"
	"github.com/piplabs/story/lib/ethclient"

	_ "cosmossdk.io/api/cosmos/tx/config/v1"          // import for side-effects
	_ "cosmossdk.io/x/upgrade"                        // import for side-effects
	_ "github.com/cosmos/cosmos-sdk/x/auth"           // import for side-effects
	_ "github.com/cosmos/cosmos-sdk/x/auth/tx/config" // import for side-effects
	_ "github.com/cosmos/cosmos-sdk/x/bank"           // import for side-effects
	_ "github.com/cosmos/cosmos-sdk/x/consensus"      // import for side-effects
	_ "github.com/cosmos/cosmos-sdk/x/distribution"   // import for side-effects
	_ "github.com/cosmos/cosmos-sdk/x/genutil"        // import for side-effects
	_ "github.com/cosmos/cosmos-sdk/x/gov"            // import for side-effects
	_ "github.com/cosmos/cosmos-sdk/x/slashing"       // import for side-effects
	_ "github.com/cosmos/cosmos-sdk/x/staking"        // import for side-effects
)

const Name = "story"

const (
	v1                    = appv1.Version
	DefaultInitialVersion = v1
)

var (
	_ runtime.AppI            = (*App)(nil)
	_ servertypes.Application = (*App)(nil)
)

// App extends an ABCI application, but with most of its parameters exported.
// They are exported for convenience in creating helper functions, as object
// capabilities aren't needed for testing.
type App struct {
	*runtime.App

	appCodec          codec.Codec
	txConfig          client.TxConfig
	interfaceRegistry codectypes.InterfaceRegistry

	Keepers keepers.Keepers

	keyVersions map[uint64][]string
	keys        map[string]*storetypes.KVStoreKey

	// override the runtime baseapp's module manager to use the custom module manager
	ModuleManager *module.Manager
	configurator  module.Configurator
}

// newApp returns a reference to an initialized App.
func newApp(
	logger log.Logger,
	db dbm.DB,
	engineCl ethclient.EngineClient,
	traceStore io.Writer,
	baseAppOpts ...func(*baseapp.BaseApp),
) (*App, error) {
	depCfg := depinject.Configs(
		DepConfig(),
		depinject.Supply(
			logger, engineCl,
		),
	)

	encodingConfig := MakeEncodingConfig(ModuleEncodingRegisters...)
	appCodec := encodingConfig.Codec
	txConfig := encodingConfig.TxConfig
	interfaceRegistry := encodingConfig.InterfaceRegistry

	var (
		app        = new(App)
		appBuilder = new(runtime.AppBuilder)
	)
	if err := depinject.Inject(depCfg,
		&appBuilder,
		&appCodec,
		&txConfig,
		&interfaceRegistry,
		&app.Keepers.AccountKeeper,
		&app.Keepers.BankKeeper,
		&app.Keepers.SignalKeeper,
		&app.Keepers.StakingKeeper,
		&app.Keepers.SlashingKeeper,
		&app.Keepers.DistrKeeper,
		&app.Keepers.ConsensusParamsKeeper,
		&app.Keepers.GovKeeper,
		&app.Keepers.EvmStakingKeeper,
		&app.Keepers.EVMEngKeeper,
		&app.Keepers.MintKeeper,
		&app.Keepers.SignalKeeper,
	); err != nil {
		return nil, errors.Wrap(err, "dep inject")
	}

	prepareOpt := func(bapp *baseapp.BaseApp) {
		// Use evm engine to create block proposals.
		// Note that we do not check MaxTxBytes since all EngineEVM transaction MUST be included since we cannot
		// postpone them to the next block. Nit: we could drop some vote extensions though...?
		bapp.SetPrepareProposal(app.Keepers.EVMEngKeeper.PrepareProposal)

		// Route proposed messages to keepers for verification and external state updates.
		bapp.SetProcessProposal(makeProcessProposalHandler(makeProcessProposalRouter(app), app.txConfig))

		// This is to set the Cosmos SDK version used by the app.
		// The app's version is set with bapp.SetProtocolVersion()
		bapp.SetVersion(version.Version)
	}
	baseAppOpts = append(baseAppOpts, prepareOpt)

	app.App = appBuilder.Build(db, traceStore, baseAppOpts...)
	app.keys = storetypes.NewKVStoreKeys(allStoreKeys()...) // TODO: ensure DI injected keys are matched here
	app.keyVersions = versionedStoreKeys()

	//app.ModuleManager.RegisterInvariants(&app.CrisisKeeper)
	app.configurator = module.NewConfigurator(app.appCodec, app.MsgServiceRouter(), app.GRPCQueryRouter())
	app.ModuleManager.RegisterServices(app.configurator)

	// NOTE: Modules can't be modified or else must be passed by reference to the module manager
	err := app.setupModuleManager()
	if err != nil {
		panic(err)
	}

	// override module orders after DI
	app.setModuleOrder()

	if err := app.Load(true); err != nil {
		return nil, errors.Wrap(err, "load app")
	}

	return app, nil
}

// EndBlocker executes application updates at the end of every block.
func (a *App) EndBlocker(ctx sdk.Context) (sdk.EndBlock, error) {
	res, err := a.ModuleManager.EndBlock(ctx)
	if err != nil {
		return sdk.EndBlock{}, errors.Wrap(err, "module manager endblocker")
	}

	currentVersion := a.AppVersion()
	if shouldUpgrade, newVersion := a.Keepers.SignalKeeper.ShouldUpgrade(ctx); shouldUpgrade {
		// Version changes must be increasing. Downgrades are not permitted
		if newVersion > currentVersion {
			a.SetProtocolVersion(newVersion)
		}
	}

	return res, nil
}

func (App) LegacyAmino() *codec.LegacyAmino {
	return nil
}

func (App) ExportAppStateAndValidators(_ bool, _, _ []string) (servertypes.ExportedApp, error) {
	return servertypes.ExportedApp{}, errors.New("not implemented")
}

func (App) SimulationManager() *module.SimulationManager {
	return nil
}

// SetCometAPI sets the comet API client.
// TODO: Figure out how to use depinject to set this.
func (a App) SetCometAPI(api comet.API) {
	a.Keepers.EVMEngKeeper.SetCometAPI(api)
}

// LoadHeight loads a particular height.
func (a *App) LoadHeight(height int64) error {
	return a.LoadVersion(height)
}

// SupportedVersions returns all the state machines that the
// application supports.
func (a *App) SupportedVersions() []uint64 {
	return a.ModuleManager.SupportedVersions()
}

// versionedKeys returns a map from moduleName to KV store key for the given app
// version.
func (a *App) versionedKeys(appVersion uint64) map[string]*storetypes.KVStoreKey {
	output := make(map[string]*storetypes.KVStoreKey)
	if keys, exists := a.keyVersions[appVersion]; exists {
		for _, moduleName := range keys {
			if key, exists := a.keys[moduleName]; exists {
				output[moduleName] = key
			}
		}
	}
	return output
}

// baseKeys returns the base keys that are mounted to every version.
func (app *App) baseKeys() map[string]*storetypes.KVStoreKey {
	return map[string]*storetypes.KVStoreKey{
		// we need to know the app version to know what stores to mount
		// thus the paramstore must always be a store that is mounted
		paramstypes.StoreKey: app.keys[paramstypes.StoreKey],
	}
}

// migrateModules performs migrations on existing modules that have registered migrations
// between versions and initializes the state of new modules for the specified app version.
func (a App) migrateModules(ctx sdk.Context, fromVersion, toVersion uint64) error {
	return a.ModuleManager.RunMigrations(ctx, a.configurator, fromVersion, toVersion)
}

func (a App) GetEvmStakingKeeper() *evmstakingkeeper.Keeper {
	return a.Keepers.EvmStakingKeeper
}

func (a App) GetStakingKeeper() *stakingkeeper.Keeper {
	return a.Keepers.StakingKeeper
}

func (a App) GetSlashingKeeper() slashingkeeper.Keeper {
	return a.Keepers.SlashingKeeper
}

func (a App) GetAccountKeeper() authkeeper.AccountKeeper {
	return a.Keepers.AccountKeeper
}

func (a App) GetBankKeeper() bankkeeper.Keeper {
	return a.Keepers.BankKeeper
}

func (a App) GetDistrKeeper() distrkeeper.Keeper {
	return a.Keepers.DistrKeeper
}

func (a App) GetUpgradeKeeper() *upgradekeeper.Keeper {
	return a.Keepers.UpgradeKeeper
}

func (a App) GetMintKeeper() mintkeeper.Keeper {
	return a.Keepers.MintKeeper
}
