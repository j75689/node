package issue

import (
	"encoding/json"
	"fmt"
	"github.com/BiJie/BinanceChain/common/log"
	"reflect"
	"strconv"
	"strings"

	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/bank"

	"github.com/BiJie/BinanceChain/common/log"
	"github.com/BiJie/BinanceChain/common/types"
	common "github.com/BiJie/BinanceChain/common/types"
	"github.com/BiJie/BinanceChain/plugins/tokens/store"
)

// NewHandler creates a new token issue message handler
func NewHandler(tokenMapper store.Mapper, keeper bank.Keeper) sdk.Handler {
	return func(ctx sdk.Context, msg sdk.Msg) sdk.Result {
		switch msg := msg.(type) {
		case IssueMsg:
			return handleIssueToken(ctx, tokenMapper, keeper, msg)
		case MintMsg:
			return handleMintToken(ctx, tokenMapper, keeper, msg)
		default:
			errMsg := "Unrecognized msg type: " + reflect.TypeOf(msg).Name()
			return sdk.ErrUnknownRequest(errMsg).Result()
		}
	}
}

func handleIssueToken(ctx sdk.Context, tokenMapper store.Mapper, bankKeeper bank.Keeper, msg IssueMsg) sdk.Result {
	errLogMsg := "issue token failed"
	symbol := strings.ToUpper(msg.Symbol)
	logger := log.With("module", "token", "symbol", symbol, "name", msg.Name, "total_supply", msg.TotalSupply, "issuer", msg.From)
	var suffix string

	// TxHashKey is set in BaseApp's runMsgs
	txHash := ctx.Value(baseapp.TxHashKey)
	if txHashStr, ok := txHash.(string); ok {
		if len(txHashStr) >= types.TokenSymbolTxHashSuffixLen {
			suffix = txHashStr[:types.TokenSymbolTxHashSuffixLen]
		} else {
			logger.Error(errLogMsg,
				"reason", fmt.Sprintf("%s on Context had a length of %d, expected >= %d",
					baseapp.TxHashKey, len(txHashStr), types.TokenSymbolTxHashSuffixLen))
			return sdk.ErrInternal(fmt.Sprintf("unable to get the %s from Context", baseapp.TxHashKey)).Result()
		}
	} else {
		logger.Error(errLogMsg,
			"reason", fmt.Sprintf("%s on Context is not a string as expected", baseapp.TxHashKey))
		return sdk.ErrInternal(fmt.Sprintf("unable to get the %s from Context", baseapp.TxHashKey)).Result()
	}

	// the symbol is suffixed with the first n bytes of the tx hash
	symbol = fmt.Sprintf("%s-%s", symbol, suffix)

	if exists := tokenMapper.Exists(ctx, symbol); exists {
		logger.Info(errLogMsg, "reason", "already exists")
		return sdk.ErrInvalidCoins(fmt.Sprintf("symbol(%s) already exists", msg.Symbol)).Result()
	}

	token, err := common.NewToken(msg.Name, symbol, msg.TotalSupply, msg.From, msg.Mintable)
	if err != nil {
		logger.Error(errLogMsg, "reason", "create token failed: "+err.Error())
		return sdk.ErrInternal(fmt.Sprintf("unable to create token struct: %s", err.Error())).Result()
	}

	if err := tokenMapper.NewToken(ctx, *token); err != nil {
		logger.Error(errLogMsg, "reason", "add token failed: "+err.Error())
		return sdk.ErrInvalidCoins(err.Error()).Result()
	}

	if _, _, sdkError := bankKeeper.AddCoins(ctx, token.Owner,
		sdk.Coins{{
			Denom:  token.Symbol,
			Amount: token.TotalSupply.ToInt64(),
		}}); sdkError != nil {
		logger.Error(errLogMsg, "reason", "update balance failed: "+sdkError.Error())
		return sdkError.Result()
	}

	serialized, err := json.Marshal(token)
	if err != nil {
		logger.Error(errLogMsg, "reason", "fatal! unable to json serialize token: "+err.Error())
		panic(err) // fatal, the sky is falling in goland
	}

	logger.Info("finished issuing token")

	return sdk.Result{
		Data: serialized,
		Log:  fmt.Sprintf("Issued %s", token.Symbol),
	}
}

func handleMintToken(ctx sdk.Context, tokenMapper store.Mapper, bankKeeper bank.Keeper, msg MintMsg) sdk.Result {
	symbol := strings.ToUpper(msg.Symbol)
	logger := log.With("module", "token", "symbol", symbol, "amount", msg.Amount, "minter", msg.From)

	errLogMsg := "mint token failed"
	token, err := tokenMapper.GetToken(ctx, symbol)
	if err != nil {
		logger.Info(errLogMsg, "reason", "symbol not exist")
		return sdk.ErrInvalidCoins(fmt.Sprintf("symbol(%s) does not exist", msg.Symbol)).Result()
	}

	if !token.Mintable {
		logger.Info(errLogMsg, "reason", "token cannot be minted")
		return sdk.ErrInvalidCoins(fmt.Sprintf("token(%s) cannot be minted", msg.Symbol)).Result()
	}

	if !token.IsOwner(msg.From) {
		logger.Info(errLogMsg, "reason", "not the token owner")
		return sdk.ErrUnauthorized(fmt.Sprintf("only the owner can mint token %s", msg.Symbol)).Result()
	}

	// use minus to prevent overflow
	if msg.Amount > common.TokenMaxTotalSupply-token.TotalSupply.ToInt64() {
		logger.Info(errLogMsg, "reason", "exceed the max total supply")
		return sdk.ErrInvalidCoins(fmt.Sprintf("mint amount is too large, the max total supply is %ds",
			common.TokenMaxTotalSupply)).Result()
	}
	newTotalSupply := token.TotalSupply.ToInt64() + msg.Amount
	err = tokenMapper.UpdateTotalSupply(ctx, symbol, newTotalSupply)
	if err != nil {
		logger.Error(errLogMsg, "reason", "update total supply failed: "+err.Error())
		return sdk.ErrInternal(fmt.Sprintf("update total supply failed")).Result()
	}

	_, _, sdkError := bankKeeper.AddCoins(ctx, token.Owner,
		sdk.Coins{{
			Denom:  token.Symbol,
			Amount: msg.Amount,
		}})
	if sdkError != nil {
		logger.Error(errLogMsg, "reason", "update balance failed: "+sdkError.Error())
		return sdkError.Result()
	}

	logger.Info("finished minting token")
	return sdk.Result{
		Data: []byte(strconv.FormatInt(newTotalSupply, 10)),
	}
}
