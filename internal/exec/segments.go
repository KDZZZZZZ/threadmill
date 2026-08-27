package exec

import (
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const maxCacheSegments = 32

type segmentCondition uint8

const (
	segmentAlways segmentCondition = iota
	segmentOnSuccess
	segmentOnFailure
)

type commandSegment struct {
	command     string
	condition   segmentCondition
	needsStatus bool
}

var statefulShellBuiltins = map[string]struct{}{
	".": {}, "alias": {}, "bind": {}, "break": {}, "builtin": {},
	"bg": {}, "cd": {}, "command": {}, "complete": {}, "compgen": {},
	"compopt": {}, "continue": {}, "declare": {}, "dirs": {}, "disown": {},
	"enable": {}, "eval": {}, "exec": {}, "exit": {}, "export": {},
	"fc": {}, "fg": {}, "getopts": {}, "hash": {}, "history": {},
	"jobs": {}, "let": {}, "local": {}, "logout": {}, "mapfile": {},
	"popd": {}, "pushd": {}, "read": {}, "readarray": {}, "readonly": {},
	"return": {}, "set": {}, "shift": {}, "shopt": {}, "source": {},
	"suspend": {}, "times": {}, "trap": {}, "typeset": {}, "ulimit": {},
	"umask": {}, "unalias": {}, "unset": {}, "wait": {},
}

// splitCacheCommand 只拆顶层 shell 列表。任何可能在列表元素间传递 shell
// 状态的语法都会让整条命令退回原有的单 shell 路径。
func splitCacheCommand(source string) []commandSegment {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(source), "")
	if err != nil || !safeToSegment(file) {
		return nil
	}
	segments := make([]commandSegment, 0, len(file.Stmts))
	for _, stmt := range file.Stmts {
		if !appendSegments(source, stmt, segmentAlways, &segments) {
			return nil
		}
	}
	if len(segments) < 2 {
		return nil
	}
	return segments
}

func appendSegments(source string, stmt *syntax.Stmt, condition segmentCondition, dst *[]commandSegment) bool {
	if binary, ok := stmt.Cmd.(*syntax.BinaryCmd); ok &&
		!stmt.Negated && !stmt.Background && !stmt.Coprocess && len(stmt.Redirs) == 0 {
		var rightCondition segmentCondition
		switch binary.Op {
		case syntax.AndStmt:
			rightCondition = segmentOnSuccess
		case syntax.OrStmt:
			rightCondition = segmentOnFailure
		default:
			return appendLeafSegment(source, stmt, condition, dst)
		}
		return appendSegments(source, binary.X, condition, dst) &&
			appendSegments(source, binary.Y, rightCondition, dst)
	}
	return appendLeafSegment(source, stmt, condition, dst)
}

func appendLeafSegment(source string, stmt *syntax.Stmt, condition segmentCondition, dst *[]commandSegment) bool {
	if len(*dst) >= maxCacheSegments {
		return false
	}
	start, end := stmt.Pos().Offset(), stmt.End().Offset()
	if stmt.Semicolon.IsValid() {
		end = stmt.Semicolon.Offset()
	}
	if start >= end || end > uint(len(source)) {
		return false
	}
	command := strings.TrimSpace(source[start:end])
	if command == "" {
		return false
	}
	*dst = append(*dst, commandSegment{
		command:     command,
		condition:   condition,
		needsStatus: usesExitStatus(stmt),
	})
	return true
}

func safeToSegment(file *syntax.File) bool {
	safe := true
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil || !safe {
			return false
		}
		switch node := node.(type) {
		case *syntax.Assign, *syntax.ArithmCmd, *syntax.ArithmExp, *syntax.CoprocClause,
			*syntax.DeclClause, *syntax.ForClause, *syntax.FuncDecl, *syntax.LetClause,
			*syntax.ProcSubst, *syntax.TestClause:
			safe = false
		case *syntax.ParamExp:
			safe = node.Param != nil && node.Param.Value == "?"
		case *syntax.Stmt:
			safe = !node.Background && !node.Coprocess
		case *syntax.Redirect:
			safe = node.Hdoc == nil && (node.N == nil || numericFD(node.N.Value))
		case *syntax.CallExpr:
			if len(node.Args) == 0 {
				safe = false
				break
			}
			name := node.Args[0].Lit()
			_, stateful := statefulShellBuiltins[name]
			safe = name != "" && !stateful &&
				!assigningPrintf(node, name) && !readsShellVariable(node, name)
		}
		return safe
	})
	return safe
}

func readsShellVariable(call *syntax.CallExpr, name string) bool {
	if name != "test" && name != "[" {
		return false
	}
	for _, arg := range call.Args[1:] {
		if strings.HasPrefix(arg.Lit(), "-v") {
			return true
		}
	}
	return false
}

func assigningPrintf(call *syntax.CallExpr, name string) bool {
	if name != "printf" {
		return false
	}
	for _, arg := range call.Args[1:] {
		if strings.HasPrefix(arg.Lit(), "-v") {
			return true
		}
	}
	return false
}

func numericFD(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func usesExitStatus(stmt *syntax.Stmt) bool {
	uses := false
	syntax.Walk(stmt, func(node syntax.Node) bool {
		if node == nil || uses {
			return false
		}
		if param, ok := node.(*syntax.ParamExp); ok && param.Param != nil && param.Param.Value == "?" {
			uses = true
		}
		return !uses
	})
	return uses
}

func (s commandSegment) runnable(exitCode int) bool {
	switch s.condition {
	case segmentOnSuccess:
		return exitCode == 0
	case segmentOnFailure:
		return exitCode != 0
	default:
		return true
	}
}

func (s commandSegment) cacheCommand(exitCode int) string {
	if !s.needsStatus {
		return s.command
	}
	return "(exit " + strconv.Itoa(exitCode) + "); " + s.command
}
