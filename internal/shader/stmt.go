// Copyright 2020 The Ebiten Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package shader

import (
	"fmt"
	"go/ast"
	gconstant "go/constant"
	"go/token"
	"strings"

	"github.com/hajimehoshi/ebiten/internal/shaderir"
)

func (cs *compileState) parseStmt(block *block, stmt ast.Stmt, inParams []variable) ([]shaderir.Stmt, bool) {
	var stmts []shaderir.Stmt

	switch stmt := stmt.(type) {
	case *ast.AssignStmt:
		switch stmt.Tok {
		case token.DEFINE:
			if len(stmt.Lhs) != len(stmt.Rhs) && len(stmt.Rhs) != 1 {
				cs.addError(stmt.Pos(), fmt.Sprintf("single-value context and multiple-value context cannot be mixed"))
				return nil, false
			}

			ss, ok := cs.assign(block, stmt.Pos(), stmt.Lhs, stmt.Rhs, true)
			if !ok {
				return nil, false
			}
			stmts = append(stmts, ss...)
		case token.ASSIGN:
			// TODO: What about the statement `a,b = b,a?`
			if len(stmt.Lhs) != len(stmt.Rhs) && len(stmt.Rhs) != 1 {
				cs.addError(stmt.Pos(), fmt.Sprintf("single-value context and multiple-value context cannot be mixed"))
				return nil, false
			}
			ss, ok := cs.assign(block, stmt.Pos(), stmt.Lhs, stmt.Rhs, false)
			if !ok {
				return nil, false
			}
			stmts = append(stmts, ss...)
		case token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN, token.REM_ASSIGN:
			var op shaderir.Op
			switch stmt.Tok {
			case token.ADD_ASSIGN:
				op = shaderir.Add
			case token.SUB_ASSIGN:
				op = shaderir.Sub
			case token.MUL_ASSIGN:
				op = shaderir.Mul
			case token.QUO_ASSIGN:
				op = shaderir.Div
			case token.REM_ASSIGN:
				op = shaderir.ModOp
			}

			rhs, _, ss, ok := cs.parseExpr(block, stmt.Rhs[0])
			if !ok {
				return nil, false
			}
			stmts = append(stmts, ss...)

			lhs, _, ss, ok := cs.parseExpr(block, stmt.Lhs[0])
			if !ok {
				return nil, false
			}
			stmts = append(stmts, ss...)

			stmts = append(stmts, shaderir.Stmt{
				Type: shaderir.Assign,
				Exprs: []shaderir.Expr{
					lhs[0],
					{
						Type: shaderir.Binary,
						Op:   op,
						Exprs: []shaderir.Expr{
							lhs[0],
							rhs[0],
						},
					},
				},
			})
		default:
			cs.addError(stmt.Pos(), fmt.Sprintf("unexpected token: %s", stmt.Tok))
		}
	case *ast.BlockStmt:
		b, ok := cs.parseBlock(block, stmt.List, inParams, nil)
		if !ok {
			return nil, false
		}
		stmts = append(stmts, shaderir.Stmt{
			Type: shaderir.BlockStmt,
			Blocks: []shaderir.Block{
				b.ir,
			},
		})
	case *ast.DeclStmt:
		ss, ok := cs.parseDecl(block, stmt.Decl)
		if !ok {
			return nil, false
		}
		stmts = append(stmts, ss...)

	case *ast.ForStmt:
		msg := "for-statement must follow this format: for (varname) := (constant); (varname) (op) (constant); (varname) (op) (constant) { ..."
		if stmt.Init == nil {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if stmt.Cond == nil {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if stmt.Post == nil {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}

		// Create a new pseudo block for the initial statement, so that the counter variable belongs to the
		// new pseudo block for each for-loop. Without this, the samely named counter variables in different
		// for-loops confuses the parser.
		pseudoBlock, ok := cs.parseBlock(block, []ast.Stmt{stmt.Init}, inParams, nil)
		if !ok {
			return nil, false
		}
		ss := pseudoBlock.ir.Stmts

		if len(ss) != 1 {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if ss[0].Type != shaderir.Assign {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if ss[0].Exprs[0].Type != shaderir.LocalVariable {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		varidx := ss[0].Exprs[0].Index
		if ss[0].Exprs[1].Type != shaderir.NumberExpr {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}

		vartype := pseudoBlock.vars[0].typ
		init := ss[0].Exprs[1].Const

		exprs, ts, ss, ok := cs.parseExpr(pseudoBlock, stmt.Cond)
		if !ok {
			return nil, false
		}
		if len(exprs) != 1 {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if len(ts) != 1 || ts[0].Main != shaderir.Bool {
			cs.addError(stmt.Pos(), "for-statement's condition must be bool")
			return nil, false
		}
		if len(ss) != 0 {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if exprs[0].Type != shaderir.Binary {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		op := exprs[0].Op
		if op != shaderir.LessThanOp && op != shaderir.LessThanEqualOp && op != shaderir.GreaterThanOp && op != shaderir.GreaterThanEqualOp && op != shaderir.EqualOp && op != shaderir.NotEqualOp {
			cs.addError(stmt.Pos(), "for-statement's condition must have one of these operators: <, <=, >, >=, ==, !=")
			return nil, false
		}
		if exprs[0].Exprs[0].Type != shaderir.LocalVariable {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if exprs[0].Exprs[0].Index != varidx {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if exprs[0].Exprs[1].Type != shaderir.NumberExpr {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		end := exprs[0].Exprs[1].Const

		postSs, ok := cs.parseStmt(pseudoBlock, stmt.Post, inParams)
		if !ok {
			return nil, false
		}
		if len(postSs) != 1 {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if postSs[0].Type != shaderir.Assign {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if postSs[0].Exprs[0].Type != shaderir.LocalVariable {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if postSs[0].Exprs[0].Index != varidx {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if postSs[0].Exprs[1].Type != shaderir.Binary {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if postSs[0].Exprs[1].Exprs[0].Type != shaderir.LocalVariable {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if postSs[0].Exprs[1].Exprs[0].Index != varidx {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		if postSs[0].Exprs[1].Exprs[1].Type != shaderir.NumberExpr {
			cs.addError(stmt.Pos(), msg)
			return nil, false
		}
		delta := postSs[0].Exprs[1].Exprs[1].Const
		switch postSs[0].Exprs[1].Op {
		case shaderir.Add:
		case shaderir.Sub:
			delta = gconstant.UnaryOp(token.SUB, delta, 0)
		default:
			cs.addError(stmt.Pos(), "for-statement's post statement must have one of these operators: +=, -=, ++, --")
			return nil, false
		}

		b, ok := cs.parseBlock(pseudoBlock, []ast.Stmt{stmt.Body}, inParams, nil)
		if !ok {
			return nil, false
		}
		bodyir := b.ir
		for len(bodyir.Stmts) == 1 && bodyir.Stmts[0].Type == shaderir.BlockStmt {
			bodyir = bodyir.Stmts[0].Blocks[0]
		}

		// As the pseudo block is not actually used, copy the variable part to the actual block.
		// This must be done after parsing the for-loop is done, or the duplicated variables confuses the
		// parsing.
		block.vars = append(block.vars, pseudoBlock.vars[0])
		block.vars[len(block.vars)-1].forLoopCounter = true

		stmts = append(stmts, shaderir.Stmt{
			Type:        shaderir.For,
			Blocks:      []shaderir.Block{bodyir},
			ForVarType:  vartype,
			ForVarIndex: varidx,
			ForInit:     init,
			ForEnd:      end,
			ForOp:       op,
			ForDelta:    delta,
		})

	case *ast.IfStmt:
		if stmt.Init != nil {
			init := stmt.Init
			stmt.Init = nil
			b, ok := cs.parseBlock(block, []ast.Stmt{init, stmt}, inParams, nil)
			if !ok {
				return nil, false
			}

			stmts = append(stmts, shaderir.Stmt{
				Type:   shaderir.BlockStmt,
				Blocks: []shaderir.Block{b.ir},
			})
			return stmts, true
		}

		exprs, ts, ss, ok := cs.parseExpr(block, stmt.Cond)
		if !ok {
			return nil, false
		}
		if len(ts) != 1 || ts[0].Main != shaderir.Bool {
			var tss []string
			for _, t := range ts {
				tss = append(tss, t.String())
			}
			cs.addError(stmt.Pos(), fmt.Sprintf("if-condition must be bool but: %s", strings.Join(tss, ", ")))
			return nil, false
		}
		stmts = append(stmts, ss...)

		var bs []shaderir.Block
		b, ok := cs.parseBlock(block, stmt.Body.List, inParams, nil)
		if !ok {
			return nil, false
		}
		bs = append(bs, b.ir)

		if stmt.Else != nil {
			switch s := stmt.Else.(type) {
			case *ast.BlockStmt:
				b, ok := cs.parseBlock(block, s.List, inParams, nil)
				if !ok {
					return nil, false
				}
				bs = append(bs, b.ir)
			default:
				b, ok := cs.parseBlock(block, []ast.Stmt{s}, inParams, nil)
				if !ok {
					return nil, false
				}
				bs = append(bs, b.ir)
			}
		}

		stmts = append(stmts, shaderir.Stmt{
			Type:   shaderir.If,
			Exprs:  exprs,
			Blocks: bs,
		})

	case *ast.IncDecStmt:
		exprs, _, ss, ok := cs.parseExpr(block, stmt.X)
		if !ok {
			return nil, false
		}
		stmts = append(stmts, ss...)
		var op shaderir.Op
		switch stmt.Tok {
		case token.INC:
			op = shaderir.Add
		case token.DEC:
			op = shaderir.Sub
		}
		stmts = append(stmts, shaderir.Stmt{
			Type: shaderir.Assign,
			Exprs: []shaderir.Expr{
				exprs[0],
				{
					Type: shaderir.Binary,
					Op:   op,
					Exprs: []shaderir.Expr{
						exprs[0],
						{
							Type:      shaderir.NumberExpr,
							Const:     gconstant.MakeInt64(1),
							ConstType: shaderir.ConstTypeInt,
						},
					},
				},
			},
		})

	case *ast.ReturnStmt:
		for i, r := range stmt.Results {
			exprs, _, ss, ok := cs.parseExpr(block, r)
			if !ok {
				return nil, false
			}
			stmts = append(stmts, ss...)
			if len(exprs) == 0 {
				continue
			}
			if len(exprs) > 1 {
				cs.addError(r.Pos(), "multiple-context with return is not implemented yet")
				continue
			}
			stmts = append(stmts, shaderir.Stmt{
				Type: shaderir.Assign,
				Exprs: []shaderir.Expr{
					{
						Type:  shaderir.LocalVariable,
						Index: len(inParams) + i,
					},
					exprs[0],
				},
			})
		}
		stmts = append(stmts, shaderir.Stmt{
			Type: shaderir.Return,
		})

	case *ast.ExprStmt:
		exprs, _, ss, ok := cs.parseExpr(block, stmt.X)
		if !ok {
			return nil, false
		}
		stmts = append(stmts, ss...)

		for _, expr := range exprs {
			if expr.Type != shaderir.Call {
				continue
			}
			stmts = append(stmts, shaderir.Stmt{
				Type:  shaderir.ExprStmt,
				Exprs: []shaderir.Expr{expr},
			})
		}

	default:
		cs.addError(stmt.Pos(), fmt.Sprintf("unexpected statement: %#v", stmt))
		return nil, false
	}
	return stmts, true
}

func (cs *compileState) assign(block *block, pos token.Pos, lhs, rhs []ast.Expr, define bool) ([]shaderir.Stmt, bool) {
	var stmts []shaderir.Stmt
	var rhsExprs []shaderir.Expr
	var rhsTypes []shaderir.Type

	for i, e := range lhs {
		if len(lhs) == len(rhs) {
			// Prase RHS first for the order of the statements.
			r, origts, ss, ok := cs.parseExpr(block, rhs[i])
			if !ok {
				return nil, false
			}
			stmts = append(stmts, ss...)

			if define {
				v := variable{
					name: e.(*ast.Ident).Name,
				}
				ts, ok := cs.functionReturnTypes(block, rhs[i])
				if !ok {
					ts = origts
				}
				if len(ts) > 1 {
					cs.addError(pos, fmt.Sprintf("single-value context and multiple-value context cannot be mixed"))
					return nil, false
				}
				if len(ts) == 1 {
					v.typ = ts[0]
				}
				block.vars = append(block.vars, v)
			}

			if len(r) > 1 {
				cs.addError(pos, fmt.Sprintf("single-value context and multiple-value context cannot be mixed"))
				return nil, false
			}

			l, _, ss, ok := cs.parseExpr(block, lhs[i])
			if !ok {
				return nil, false
			}
			stmts = append(stmts, ss...)

			if r[0].Type == shaderir.NumberExpr {
				t, ok := block.findLocalVariableByIndex(l[0].Index)
				if !ok {
					cs.addError(pos, fmt.Sprintf("unexpected local variable index: %d", l[0].Index))
					return nil, false
				}
				switch t.Main {
				case shaderir.Int:
					r[0].ConstType = shaderir.ConstTypeInt
				case shaderir.Float:
					r[0].ConstType = shaderir.ConstTypeFloat
				}
			}

			stmts = append(stmts, shaderir.Stmt{
				Type:  shaderir.Assign,
				Exprs: []shaderir.Expr{l[0], r[0]},
			})
		} else {
			if i == 0 {
				var ss []shaderir.Stmt
				var ok bool
				rhsExprs, rhsTypes, ss, ok = cs.parseExpr(block, rhs[0])
				if !ok {
					return nil, false
				}
				if len(rhsExprs) != len(lhs) {
					cs.addError(pos, fmt.Sprintf("single-value context and multiple-value context cannot be mixed"))
				}
				stmts = append(stmts, ss...)
			}

			if define {
				v := variable{
					name: e.(*ast.Ident).Name,
				}
				v.typ = rhsTypes[i]
				block.vars = append(block.vars, v)
			}

			l, _, ss, ok := cs.parseExpr(block, lhs[i])
			if !ok {
				return nil, false
			}
			stmts = append(stmts, ss...)

			stmts = append(stmts, shaderir.Stmt{
				Type:  shaderir.Assign,
				Exprs: []shaderir.Expr{l[0], rhsExprs[i]},
			})
		}
	}
	return stmts, true
}
