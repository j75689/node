package order

import (
	"fmt"
	"reflect"
	"strings"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"

	common "github.com/BiJie/BinanceChain/common/types"
	"github.com/BiJie/BinanceChain/common/utils"
	me "github.com/BiJie/BinanceChain/plugins/dex/matcheng"
	"github.com/BiJie/BinanceChain/plugins/dex/types"
)

// NewHandler - returns a handler for dex type messages.
func NewHandler(k Keeper, accountMapper auth.AccountMapper) sdk.Handler {
	return func(ctx sdk.Context, msg sdk.Msg) sdk.Result {
		switch msg := msg.(type) {
		case NewOrderMsg:
			return handleNewOrder(ctx, k, accountMapper, msg)
		case CancelOrderMsg:
			return handleCancelOrder(ctx, k, accountMapper, msg)
		default:
			errMsg := fmt.Sprintf("Unrecognized dex msg type: %v", reflect.TypeOf(msg).Name())
			return sdk.ErrUnknownRequest(errMsg).Result()
		}
	}
}

// TODO: duplicated with plugins/tokens/freeze/handler.go
func updateLockedOfAccount(ctx sdk.Context, accountMapper auth.AccountMapper, address sdk.AccAddress, symbol string, lockedAmount int64) {
	account := accountMapper.GetAccount(ctx, address).(common.NamedAccount)
	account.SetLockedCoins(account.GetLockedCoins().Plus(append(sdk.Coins{}, sdk.Coin{Denom: symbol, Amount: sdk.NewInt(lockedAmount)})))
	accountMapper.SetAccount(ctx, account)
}

func handleNewOrder(ctx sdk.Context, keeper Keeper, accountMapper auth.AccountMapper, msg NewOrderMsg) sdk.Result {
	// TODO: the below is mostly copied from FreezeToken. It should be rewritten once "locked" becomes a field on account
	_, ok := keeper.OrderExists(msg.Id)
	if ctx.IsCheckTx() {
		//only check whether there exists order to cancel
		if ok {
			errString := fmt.Sprintf("Duplicated order [%v] on symbol [%v]", msg.Id, msg.Symbol)
			return sdk.NewError(types.DefaultCodespace, types.CodeDuplicatedOrder, errString).Result()
		}
	}
	var amountToLock int64
	tradeCcy, quoteCcy, _ := utils.TradeSymbol2Ccy(msg.Symbol)
	var symbolToLock string
	if msg.Side == Side.BUY {
		// TODO: where is 10^8 stored?
		amountToLock = utils.CalBigNotional(msg.Quantity, msg.Price)
		symbolToLock = strings.ToUpper(quoteCcy)
	} else {
		amountToLock = msg.Quantity
		symbolToLock = strings.ToUpper(tradeCcy)
	}
	coins := keeper.ck.GetCoins(ctx, msg.Sender)
	if coins.AmountOf(symbolToLock).Int64() < amountToLock {
		return sdk.ErrInsufficientCoins("do not have enough token to lock").Result()
	}

	// TODO: perform reduce avail + increase locked + insert orderbook atomically
	_, _, sdkError := keeper.ck.SubtractCoins(ctx, msg.Sender, append((sdk.Coins)(nil), sdk.Coin{Denom: symbolToLock, Amount: sdk.NewInt(amountToLock)}))
	if sdkError != nil {
		return sdkError.Result()
	}

	updateLockedOfAccount(ctx, accountMapper, msg.Sender, symbolToLock, amountToLock)

	if !ctx.IsCheckTx() { // only insert into order book during DeliverTx
		err := keeper.AddOrder(msg, ctx.BlockHeight())
		if err != nil {
			return sdk.NewError(types.DefaultCodespace, types.CodeFailInsertOrder, err.Error()).Result()
		}
	}
	return sdk.Result{}
}

// Handle CancelOffer -
func handleCancelOrder(ctx sdk.Context, keeper Keeper, accountMapper auth.AccountMapper, msg CancelOrderMsg) sdk.Result {
	origOrd, ok := keeper.OrderExists(msg.RefId)

	//only check whether there exists order to cancel
	if !ok {
		errString := fmt.Sprintf("Failed to find order [%v]", msg.RefId)
		return sdk.NewError(types.DefaultCodespace, types.CodeFailLocateOrderToCancel, errString).Result()
	}

	// only can cancel their own order
	if !reflect.DeepEqual(msg.Sender, origOrd.Sender) {
		errString := fmt.Sprintf("Order [%v] does not belong to transaction sender", msg.RefId)
		return sdk.NewError(types.DefaultCodespace, types.CodeFailLocateOrderToCancel, errString).Result()
	}

	var ord me.OrderPart
	var err error
	if !ctx.IsCheckTx() {
		//remove order from cache and order book
		ord, err = keeper.RemoveOrder(origOrd.Id, origOrd.Symbol, origOrd.Side, origOrd.Price)
	} else {
		ord, err = keeper.GetOrder(origOrd.Id, origOrd.Symbol, origOrd.Side, origOrd.Price)
	}
	if err != nil {
		return sdk.NewError(types.DefaultCodespace, types.CodeFailLocateOrderToCancel, err.Error()).Result()
	}
	//unlocked the locked qty for the unfilled qty
	unlockAmount := ord.LeavesQty()

	tradeCcy, quoteCcy, _ := utils.TradeSymbol2Ccy(origOrd.Symbol)
	var symbolToUnlock string
	if origOrd.Side == Side.BUY {
		symbolToUnlock = strings.ToUpper(quoteCcy)
		unlockAmount = utils.CalBigNotional(origOrd.Price, unlockAmount)
	} else {
		symbolToUnlock = strings.ToUpper(tradeCcy)
	}
	account := accountMapper.GetAccount(ctx, msg.Sender).(common.NamedAccount)
	lockedAmount := account.GetLockedCoins().AmountOf(symbolToUnlock).Int64()
	if lockedAmount < unlockAmount {
		return sdk.ErrInsufficientCoins("do not have enough token to unlock").Result()
	}

	_, _, sdkError := keeper.ck.AddCoins(ctx, msg.Sender, append((sdk.Coins)(nil), sdk.Coin{Denom: symbolToUnlock, Amount: sdk.NewInt(unlockAmount)}))

	if sdkError != nil {
		return sdkError.Result()
	}

	updateLockedOfAccount(ctx, accountMapper, msg.Sender, symbolToUnlock, -unlockAmount)

	//TODO: here fee should be calculated and deducted
	return sdk.Result{}
}