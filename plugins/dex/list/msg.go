package list

import (
	"encoding/json"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/BiJie/BinanceChain/common/types"
)

const Route = "dexList"

type Msg struct {
	From             sdk.AccAddress `json:"from"`
	BaseAssetSymbol  string         `json:"base_asset_symbol"`
	QuoteAssetSymbol string         `json:"quote_asset_symbol"`
	InitPrice        int64          `json:"init_price"`
}

func NewMsg(from sdk.AccAddress, baseAssetSymbol string, quoteAssetSymbol string, initPrice int64) Msg {
	return Msg{
		From:             from,
		BaseAssetSymbol:  baseAssetSymbol,
		QuoteAssetSymbol: quoteAssetSymbol,
		InitPrice:        initPrice,
	}
}

func (msg Msg) Type() string                            { return Route }
func (msg Msg) String() string                          { return fmt.Sprintf("MsgList{%#v}", msg) }
func (msg Msg) Get(key interface{}) (value interface{}) { return nil }
func (msg Msg) GetSigners() []sdk.AccAddress            { return []sdk.AccAddress{msg.From} }

func (msg Msg) ValidateBasic() sdk.Error {
	err := types.ValidateSymbol(msg.BaseAssetSymbol)
	if err != nil {
		return sdk.ErrInvalidCoins("base token: " + err.Error())
	}

	err = types.ValidateSymbol(msg.QuoteAssetSymbol)
	if err != nil {
		return sdk.ErrInvalidCoins("quote token: " + err.Error())
	}

	if msg.InitPrice <= 0 {
		return sdk.ErrInvalidCoins("price should be positive")
	}

	return nil
}

func (msg Msg) GetSignBytes() []byte {
	b, err := json.Marshal(msg)
	if err != nil {
		panic(err)
	}
	return b
}
