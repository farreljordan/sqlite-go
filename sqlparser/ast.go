package sqlparser

import "strings"

// Statement is the top-level interface every parsed statement implements.
type Statement interface {
	statementNode()
}

// Select represents: SELECT <exprs> FROM <table>
type Select struct {
	SelectExprs []SelectExpr
	From        TableName
	Where       *WhereClause
}

func (*Select) statementNode() {}

// SelectExpr is one item in the SELECT list.
type SelectExpr interface {
	selectExprNode()
}

// StarExpr represents "*" in SELECT *.
type StarExpr struct{}

func (*StarExpr) selectExprNode() {}

// ColExpr represents a plain column name in the SELECT list.
type ColExpr struct {
	Name string
}

func (*ColExpr) selectExprNode() {}

// FuncExpr represents a function call like COUNT(*).
type FuncExpr struct {
	Name string // e.g. "COUNT"
	Arg  string // e.g. "*"
}

func (*FuncExpr) selectExprNode() {}

// TableName is the table reference after FROM.
type TableName struct {
	Name string
}

type WhereClause struct {
	Column string
	Value  string
}

// IsCountStar reports whether a SelectExpr is COUNT(*).
func IsCountStar(e SelectExpr) bool {
	f, ok := e.(*FuncExpr)
	return ok && strings.ToUpper(f.Name) == "COUNT" && f.Arg == "*"
}
