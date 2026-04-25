package polymarket

import (
	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// TradeEnv is the evaluation environment for user-defined unusual-trade expressions.
// Field names (capitalized) are the variable names available in expressions,
// e.g. Size > 15000 && Side == "BUY".
type TradeEnv struct {
	Size       float64 // trade size in USD
	Price      float64 // trade price (0–1)
	Side       string  // "BUY" or "SELL"
	Avg        float64 // rolling average size before this trade (partial until WindowFull)
	WindowFull bool    // true once the rolling window has seen at least N trades
	AssetID    string  // CLOB token ID
	Market     string  // market condition ID
}

// CompiledExpr wraps a compiled expression program for unusual-trade detection.
type CompiledExpr struct {
	prog *vm.Program
}

// CompileExpr compiles an expression string.
// Returns an error if the expression is syntactically invalid or does not return bool.
func CompileExpr(exprStr string) (*CompiledExpr, error) {
	prog, err := expr.Compile(exprStr, expr.Env(TradeEnv{}), expr.AsBool())
	if err != nil {
		return nil, err
	}
	return &CompiledExpr{prog: prog}, nil
}

// Eval runs the compiled expression against the given trade environment.
func (c *CompiledExpr) Eval(env TradeEnv) (bool, error) {
	out, err := expr.Run(c.prog, env)
	if err != nil {
		return false, err
	}
	return out.(bool), nil
}
