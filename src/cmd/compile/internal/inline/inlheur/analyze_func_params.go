// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package inlheur

import (
	"cmd/compile/internal/ir"
	"fmt"
	"os"
)

// paramsAnalyzer holds state information for the phase that computes
// flags for a Go functions parameters, for use in inline heuristics.
// Note that the params slice below includes entries for blanks.
type paramsAnalyzer struct {
	fname  string
	values []ParamPropBits
	params []*ir.Name
	top    []bool
	*condLevelTracker
}

// dclParams returns a slice containing the non-blank, named params
// for the specific function (plus rcvr as well if applicable) in
// declaration order.
func dclParams(fn *ir.Func) []*ir.Name {
	params := []*ir.Name{}
	for _, n := range fn.Dcl {
		if n.Op() != ir.ONAME {
			continue
		}
		if n.Class != ir.PPARAM {
			continue
		}
		params = append(params, n)
	}
	return params
}

// getParams returns an *ir.Name slice containing all params for the
// function (plus rcvr as well if applicable). Note that this slice
// includes entries for blanks; entries in the returned slice corresponding
// to blanks or unnamed params will be nil.
func getParams(fn *ir.Func) []*ir.Name {
	dclparms := dclParams(fn)
	dclidx := 0
	recvrParms := fn.Type().RecvParams()
	params := make([]*ir.Name, len(recvrParms))
	for i := range recvrParms {
		var v *ir.Name
		if recvrParms[i].Sym != nil &&
			!recvrParms[i].Sym.IsBlank() {
			v = dclparms[dclidx]
			dclidx++
		}
		params[i] = v
	}
	return params
}

func makeParamsAnalyzer(fn *ir.Func) *paramsAnalyzer {
	params := getParams(fn) // includes receiver if applicable
	vals := make([]ParamPropBits, len(params))
	top := make([]bool, len(params))
	for i, pn := range params {
		if pn == nil {
			continue
		}
		pt := pn.Type()
		if !pt.IsScalar() && !pt.HasNil() {
			// existing properties not applicable here (for things
			// like structs, arrays, slices, etc).
			continue
		}
		// If param is reassigned, skip it.
		if ir.Reassigned(pn) {
			continue
		}
		top[i] = true
	}

	if debugTrace&debugTraceParams != 0 {
		fmt.Fprintf(os.Stderr, "=-= param analysis of func %v:\n",
			fn.Sym().Name)
		for i := range vals {
			n := "_"
			if params[i] != nil {
				n = params[i].Sym().String()
			}
			fmt.Fprintf(os.Stderr, "=-=  %d: %q %s\n",
				i, n, vals[i].String())
		}
	}

	return &paramsAnalyzer{
		fname:            fn.Sym().Name,
		values:           vals,
		params:           params,
		top:              top,
		condLevelTracker: new(condLevelTracker),
	}
}

func (pa *paramsAnalyzer) setResults(fp *FuncProps) {
	fp.ParamFlags = pa.values
}

func (pa *paramsAnalyzer) findParamIdx(n *ir.Name) int {
	if n == nil {
		panic("bad")
	}
	for i := range pa.params {
		if pa.params[i] == n {
			return i
		}
	}
	return -1
}

type testfType func(x ir.Node, param *ir.Name, idx int) (bool, bool)

// paramsAnalyzer invokes function 'testf' on the specified expression
// 'x' for each parameter, and if the result is TRUE, or's 'flag' into
// the flags for that param.
func (pa *paramsAnalyzer) checkParams(x ir.Node, flag ParamPropBits, mayflag ParamPropBits, testf testfType) {
	for idx, p := range pa.params {
		if !pa.top[idx] && pa.values[idx] == ParamNoInfo {
			continue
		}
		result, may := testf(x, p, idx)
		if debugTrace&debugTraceParams != 0 {
			fmt.Fprintf(os.Stderr, "=-= test expr %v param %s result=%v flag=%s\n", x, p.Sym().Name, result, flag.String())
		}
		if result {
			v := flag
			if pa.condLevel != 0 || may {
				v = mayflag
			}
			pa.values[idx] |= v
			pa.top[idx] = false
		}
	}
}

// foldCheckParams checks expression 'x' (an 'if' condition or
// 'switch' stmt expr) to see if the expr would fold away if a
// specific parameter had a constant value.
func (pa *paramsAnalyzer) foldCheckParams(x ir.Node) {
	pa.checkParams(x, ParamFeedsIfOrSwitch, ParamMayFeedIfOrSwitch,
		func(x ir.Node, p *ir.Name, idx int) (bool, bool) {
			return ShouldFoldIfNameConstant(x, []*ir.Name{p}), false
		})
}

// callCheckParams examines the target of call expression 'ce' to see
// if it is making a call to the value passed in for some parameter.
func (pa *paramsAnalyzer) callCheckParams(ce *ir.CallExpr) {
	switch ce.Op() {
	case ir.OCALLINTER:
		if ce.Op() != ir.OCALLINTER {
			return
		}
		sel := ce.X.(*ir.SelectorExpr)
		r := ir.StaticValue(sel.X)
		if r.Op() != ir.ONAME {
			return
		}
		name := r.(*ir.Name)
		if name.Class != ir.PPARAM {
			return
		}
		pa.checkParams(r, ParamFeedsInterfaceMethodCall,
			ParamMayFeedInterfaceMethodCall,
			func(x ir.Node, p *ir.Name, idx int) (bool, bool) {
				name := x.(*ir.Name)
				return name == p, false
			})
	case ir.OCALLFUNC:
		if ce.X.Op() != ir.ONAME {
			return
		}
		called := ir.StaticValue(ce.X)
		if called.Op() != ir.ONAME {
			return
		}
		name := called.(*ir.Name)
		if name.Class == ir.PPARAM {
			pa.checkParams(called, ParamFeedsIndirectCall,
				ParamMayFeedIndirectCall,
				func(x ir.Node, p *ir.Name, idx int) (bool, bool) {
					name := x.(*ir.Name)
					return name == p, false
				})
		} else {
			cname, isFunc, _ := isFuncName(called)
			if isFunc {
				pa.deriveFlagsFromCallee(ce, cname.Func)
			}
		}
	}
}

// deriveFlagsFromCallee tries to derive flags for the current
// function based on a call this function makes to some other
// function. Example:
//
//	/* Simple */                /* Derived from callee */
//	func foo(f func(int)) {     func foo(f func(int)) {
//	  f(2)                        bar(32, f)
//	}                           }
//	                            func bar(x int, f func()) {
//	                              f(x)
//	                            }
//
// Here we can set the "param feeds indirect call" flag for
// foo's param 'f' since we know that bar has that flag set for
// its second param, and we're passing that param a function.
func (pa *paramsAnalyzer) deriveFlagsFromCallee(ce *ir.CallExpr, callee *ir.Func) {
	calleeProps := propsForFunc(callee)
	if calleeProps == nil {
		return
	}
	if debugTrace&debugTraceParams != 0 {
		fmt.Fprintf(os.Stderr, "=-= callee props for %v:\n%s",
			callee.Sym().Name, calleeProps.String())
	}

	must := []ParamPropBits{ParamFeedsInterfaceMethodCall, ParamFeedsIndirectCall, ParamFeedsIfOrSwitch}
	may := []ParamPropBits{ParamMayFeedInterfaceMethodCall, ParamMayFeedIndirectCall, ParamMayFeedIfOrSwitch}

	for pidx, arg := range ce.Args {
		// Does the callee param have any interesting properties?
		// If not we can skip this one.
		pflag := calleeProps.ParamFlags[pidx]
		if pflag == 0 {
			continue
		}
		// See if one of the caller's parameters is flowing unmodified
		// into this actual expression.
		r := ir.StaticValue(arg)
		if r.Op() != ir.ONAME {
			return
		}
		name := r.(*ir.Name)
		if name.Class != ir.PPARAM {
			return
		}
		callerParamIdx := pa.findParamIdx(name)
		if callerParamIdx == -1 || pa.params[callerParamIdx] == nil {
			panic("something went wrong")
		}
		if !pa.top[callerParamIdx] &&
			pa.values[callerParamIdx] == ParamNoInfo {
			continue
		}
		if debugTrace&debugTraceParams != 0 {
			fmt.Fprintf(os.Stderr, "=-= pflag for arg %d is %s\n",
				pidx, pflag.String())
		}
		for i := range must {
			mayv := may[i]
			mustv := must[i]
			if pflag&mustv != 0 && pa.condLevel == 0 {
				pa.values[callerParamIdx] |= mustv
			} else if pflag&(mustv|mayv) != 0 {
				pa.values[callerParamIdx] |= mayv
			}
		}
		pa.top[callerParamIdx] = false
	}
}

func (pa *paramsAnalyzer) nodeVisitPost(n ir.Node) {
	if len(pa.values) == 0 {
		return
	}
	pa.condLevelTracker.post(n)
	switch n.Op() {
	case ir.OCALLFUNC:
		ce := n.(*ir.CallExpr)
		pa.callCheckParams(ce)
	case ir.OCALLINTER:
		ce := n.(*ir.CallExpr)
		pa.callCheckParams(ce)
	case ir.OIF:
		ifst := n.(*ir.IfStmt)
		pa.foldCheckParams(ifst.Cond)
	case ir.OSWITCH:
		swst := n.(*ir.SwitchStmt)
		if swst.Tag != nil {
			pa.foldCheckParams(swst.Tag)
		}
	}
}

func (pa *paramsAnalyzer) nodeVisitPre(n ir.Node) {
	if len(pa.values) == 0 {
		return
	}
	pa.condLevelTracker.pre(n)
}

// condLevelTracker helps keeps track very roughly of "level of conditional
// nesting", e.g. how many "if" statements you have to go through to
// get to the point where a given stmt executes. Example:
//
//	                      cond nesting level
//	func foo() {
//	 G = 1                   0
//	 if x < 10 {             0
//	  if y < 10 {            1
//	   G = 0                 2
//	  }
//	 }
//	}
//
// The intent here is to provide some sort of very abstract relative
// hotness metric, e.g. "G = 1" above is expected to be executed more
// often than "G = 0" (in the aggregate, across large numbers of
// functions).
type condLevelTracker struct {
	condLevel int
}

func (c *condLevelTracker) pre(n ir.Node) {
	// Increment level of "conditional testing" if we see
	// an "if" or switch statement, and decrement if in
	// a loop.
	switch n.Op() {
	case ir.OIF, ir.OSWITCH:
		c.condLevel++
	case ir.OFOR, ir.ORANGE:
		c.condLevel--
	}
}

func (c *condLevelTracker) post(n ir.Node) {
	switch n.Op() {
	case ir.OFOR, ir.ORANGE:
		c.condLevel++
	case ir.OIF:
		c.condLevel--
	case ir.OSWITCH:
		c.condLevel--
	}
}
