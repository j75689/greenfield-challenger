package executor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	_ "encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/bnb-chain/gnfd-challenger/common"
	"github.com/bnb-chain/gnfd-challenger/config"
	"github.com/bnb-chain/gnfd-challenger/logging"
	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/avast/retry-go/v4"
	"github.com/cosmos/cosmos-sdk/types/tx"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/evmos/ethermint/crypto/ethsecp256k1"
	rpcclient "github.com/tendermint/tendermint/rpc/client"
	"github.com/tendermint/tendermint/rpc/client/http"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	libclient "github.com/tendermint/tendermint/rpc/jsonrpc/client"
	tmtypes "github.com/tendermint/tendermint/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type ExecutorClient struct {
	rpcClient  rpcclient.Client
	txClient   tx.ServiceClient
	authClient authtypes.QueryClient
	Provider   string
	Height     uint64
	UpdatedAt  time.Time
}

type Executor struct {
	mutex             sync.RWMutex
	clientIdx         int
	greenfieldClients []*ExecutorClient
	config            *config.Config
	privateKey        *ethsecp256k1.PrivKey
	address           string
	validators        []*tmtypes.Validator // used to cache validators
	cdc               *codec.ProtoCodec
}

func grpcConn(addr string) *grpc.ClientConn {
	conn, err := grpc.Dial(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		panic(err)
	}
	return conn
}

func NewRpcClient(addr string) *http.HTTP {
	httpClient, err := libclient.DefaultHTTPClient(addr)
	if err != nil {
		panic(err)
	}
	rpcClient, err := http.NewWithClient(addr, "/websocket", httpClient)
	if err != nil {
		panic(err)
	}
	return rpcClient
}

func getGreenfieldPrivateKey(cfg *config.GreenfieldConfig) *ethsecp256k1.PrivKey {
	var privateKey string
	if cfg.KeyType == config.KeyTypeAWSPrivateKey {
		result, err := config.GetSecret(cfg.AWSSecretName, cfg.AWSRegion)
		if err != nil {
			panic(err)
		}
		type AwsPrivateKey struct {
			PrivateKey string `json:"private_key"`
		}
		var awsPrivateKey AwsPrivateKey
		err = json.Unmarshal([]byte(result), &awsPrivateKey)
		if err != nil {
			panic(err)
		}
		privateKey = awsPrivateKey.PrivateKey
	} else {
		privateKey = cfg.PrivateKey
	}
	privKey, err := HexToEthSecp256k1PrivKey(privateKey)
	if err != nil {
		panic(err)
	}
	return privKey
}

func initGreenfieldClients(rpcAddrs, grpcAddrs []string) []*ExecutorClient {
	greenfieldClients := make([]*ExecutorClient, 0)

	for i := 0; i < len(rpcAddrs); i++ {
		conn := grpcConn(grpcAddrs[i])
		greenfieldClients = append(greenfieldClients, &ExecutorClient{
			txClient:   tx.NewServiceClient(conn),
			authClient: authtypes.NewQueryClient(conn),
			rpcClient:  NewRpcClient(rpcAddrs[i]),
			Provider:   rpcAddrs[i],
			UpdatedAt:  time.Now(),
		})
	}
	return greenfieldClients
}

func NewGreenfieldExecutor(cfg *config.Config) *Executor {
	privKey := getGreenfieldPrivateKey(&cfg.GreenfieldConfig)
	return &Executor{
		clientIdx:         0,
		greenfieldClients: initGreenfieldClients(cfg.GreenfieldConfig.RPCAddrs, cfg.GreenfieldConfig.GRPCAddrs),
		privateKey:        privKey,
		address:           privKey.PubKey().Address().String(),
		config:            cfg,
		cdc:               Cdc(),
	}
}

func (e *Executor) getRpcClient() rpcclient.Client {
	e.mutex.RLock()
	defer e.mutex.RUnlock()
	return e.greenfieldClients[e.clientIdx].rpcClient
}

func (e *Executor) getTxClient() tx.ServiceClient {
	e.mutex.RLock()
	defer e.mutex.RUnlock()
	return e.greenfieldClients[e.clientIdx].txClient
}

func (e *Executor) getAuthClient() authtypes.QueryClient {
	e.mutex.RLock()
	defer e.mutex.RUnlock()
	return e.greenfieldClients[e.clientIdx].authClient
}

func (e *Executor) GetBlockResultAtHeight(height int64) (*ctypes.ResultBlockResults, error) {
	blockResults, err := e.getRpcClient().BlockResults(context.Background(), &height)
	if err != nil {
		return nil, err
	}
	return blockResults, nil
}

func (e *Executor) GetBlockAtHeight(height int64) (*tmtypes.Block, error) {
	block, err := e.getRpcClient().Block(context.Background(), &height)
	if err != nil {
		return nil, err
	}
	return block.Block, nil
}

func (e *Executor) GetLatestBlockHeightWithRetry() (latestHeight uint64, err error) {
	return e.getLatestBlockHeightWithRetry(e.getRpcClient())
}

func (e *Executor) getLatestBlockHeightWithRetry(client rpcclient.Client) (latestHeight uint64, err error) {
	return latestHeight, retry.Do(func() error {
		latestHeightQueryCtx, cancelLatestHeightQueryCtx := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelLatestHeightQueryCtx()
		var err error
		latestHeight, err = e.getLatestBlockHeight(latestHeightQueryCtx, client)
		return err
	}, common.RtyAttem,
		common.RtyDelay,
		common.RtyErr,
		retry.OnRetry(func(n uint, err error) {
			logging.Logger.Infof("failed to query latest height, attempt: %d times, max_attempts: %d", n+1, common.RtyAttem)
		}))
}

func (e *Executor) getLatestBlockHeight(ctx context.Context, client rpcclient.Client) (uint64, error) {
	status, err := client.Status(ctx)
	if err != nil {
		return 0, err
	}
	return uint64(status.SyncInfo.LatestBlockHeight), nil
}

func (e *Executor) UpdateClientLoop() {
	ticker := time.NewTicker(SleepSecondForUpdateClient * time.Second)
	for range ticker.C {
		logging.Logger.Infof("start to monitor greenfield data-seeds healthy")
		for _, greenfieldClient := range e.greenfieldClients {
			if time.Since(greenfieldClient.UpdatedAt).Seconds() > DataSeedDenyServiceThreshold {
				msg := fmt.Sprintf("data seed %s is not accessable", greenfieldClient.Provider)
				logging.Logger.Error(msg)
				//config.SendTelegramMessage(e.config.AlertConfig.Identity, e.config.AlertConfig.TelegramBotId,
				//	e.config.AlertConfig.TelegramChatId, msg)
			}
			height, err := e.getLatestBlockHeightWithRetry(greenfieldClient.rpcClient)
			if err != nil {
				logging.Logger.Errorf("get latest block height error, err=%s", err.Error())
				continue
			}
			greenfieldClient.Height = height
			greenfieldClient.UpdatedAt = time.Now()
		}
		highestHeight := uint64(0)
		highestIdx := 0
		for idx := 0; idx < len(e.greenfieldClients); idx++ {
			if e.greenfieldClients[idx].Height > highestHeight {
				highestHeight = e.greenfieldClients[idx].Height
				highestIdx = idx
			}
		}
		// current ExecutorClient block sync is fall behind, switch to the ExecutorClient with the highest block height
		if e.greenfieldClients[e.clientIdx].Height+FallBehindThreshold < highestHeight {
			e.mutex.Lock()
			e.clientIdx = highestIdx
			e.mutex.Unlock()
		}
	}
}

func (e *Executor) QueryTendermintLightBlock(height int64) ([]byte, error) {
	validators, err := e.getRpcClient().Validators(context.Background(), &height, nil, nil)
	commit, err := e.getRpcClient().Commit(context.Background(), &height)
	if err != nil {
		return nil, err
	}
	validatorSet := tmtypes.NewValidatorSet(validators.Validators)
	if err != nil {
		return nil, err
	}
	lightBlock := tmtypes.LightBlock{
		SignedHeader: &commit.SignedHeader,
		ValidatorSet: validatorSet,
	}
	protoBlock, err := lightBlock.ToProto()
	if err != nil {
		return nil, err
	}
	return protoBlock.Marshal()
}

func (e *Executor) queryLatestValidators() ([]*tmtypes.Validator, error) {
	validators, err := e.getRpcClient().Validators(context.Background(), nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return validators.Validators, nil
}

func (e *Executor) QueryValidatorsAtHeight(height uint64) ([]*tmtypes.Validator, error) {
	atHeight := int64(height)
	validators, err := e.getRpcClient().Validators(context.Background(), &atHeight, nil, nil)
	if err != nil {
		return nil, err
	}
	return validators.Validators, nil

}

func (e *Executor) QueryCachedLatestValidators() ([]*tmtypes.Validator, error) {
	if len(e.validators) != 0 {
		return e.validators, nil
	}
	validators, err := e.queryLatestValidators()
	if err != nil {
		return nil, err
	}
	return validators, nil
}

func (e *Executor) UpdateCachedLatestValidatorsLoop() {
	ticker := time.NewTicker(UpdateCachedValidatorsInterval)
	for range ticker.C {
		validators, err := e.queryLatestValidators()
		if err != nil {
			logging.Logger.Errorf("update latest greenfield validators error, err=%s", err)
			continue
		}
		e.validators = validators
	}
}

func (e *Executor) GetValidatorsBlsPublicKey() ([]string, error) {
	validators, err := e.QueryCachedLatestValidators()
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, v := range validators {
		keys = append(keys, hex.EncodeToString(v.RelayerBlsKey))
	}
	return keys, nil
}

func (e *Executor) GetAccount(address string) (authtypes.AccountI, error) {
	authRes, err := e.getAuthClient().Account(context.Background(), &authtypes.QueryAccountRequest{Address: address})
	if err != nil {
		return nil, err
	}
	var account authtypes.AccountI
	if err := e.cdc.InterfaceRegistry().UnpackAny(authRes.Account, &account); err != nil {
		return nil, err
	}
	return account, nil
}