package policy

import (
	"path"
	"regexp"
	"strings"
)

// This file finds the paths a shell command writes. The hook blocks only
// those, so a command that merely *mentions* a protected path (in a heredoc
// body, a commit message, a doc comment, or by running harness/factory) is
// allowed. It is still a best-effort parser: the integrity check at the gate
// remains the authoritative enforcement.

type tokKind int

const (
	tWord tokKind = iota
	tOp
)

type token struct {
	kind tokKind
	val  string   // unquoted word, or the operator
	subs []string // command substitutions inside a word: $(...) and `...`
	body string   // heredoc body, for << and <<- operators
}

var opList = []string{"&>>", "<<<", "<<-", "&&", "||", "&>", ">>", ">|", ">&", "<<", "<>", "|&", ">", "<", "|", "&", ";", "(", ")"}

func isOpStart(r rune) bool { return strings.ContainsRune(";|&<>()", r) }

type lexer struct {
	s       []rune
	i       int
	out     []token
	pending []struct {
		idx   int
		delim string
		tabs  bool
	}
	wantDelim *struct {
		idx  int
		tabs bool
	}
}

func lex(src string) []token {
	l := &lexer{s: []rune(src)}
	for l.i < len(l.s) {
		r := l.s[l.i]
		switch {
		case r == ' ' || r == '\t' || r == '\r':
			l.i++
		case r == '\\' && l.i+1 < len(l.s) && l.s[l.i+1] == '\n':
			l.i += 2 // line continuation
		case r == '\n':
			l.i++
			l.out = append(l.out, token{kind: tOp, val: "\n"})
			l.readHeredocs()
		case r == '#':
			for l.i < len(l.s) && l.s[l.i] != '\n' {
				l.i++
			}
		case isOpStart(r) || (r >= '0' && r <= '9' && l.fdRedirect()):
			l.op()
		default:
			l.word()
		}
	}
	return l.out
}

// fdRedirect reports whether digits at the cursor are an fd prefix (2>, 1>>).
func (l *lexer) fdRedirect() bool {
	j := l.i
	for j < len(l.s) && l.s[j] >= '0' && l.s[j] <= '9' {
		j++
	}
	return j < len(l.s) && (l.s[j] == '>' || l.s[j] == '<')
}

func (l *lexer) op() {
	start := l.i
	for l.i < len(l.s) && l.s[l.i] >= '0' && l.s[l.i] <= '9' {
		l.i++
	}
	fd := string(l.s[start:l.i])
	rest := string(l.s[l.i:])
	for _, op := range opList {
		if strings.HasPrefix(rest, op) {
			l.i += len([]rune(op))
			l.out = append(l.out, token{kind: tOp, val: fd + op})
			if op == "<<" || op == "<<-" {
				l.wantDelim = &struct {
					idx  int
					tabs bool
				}{len(l.out) - 1, op == "<<-"}
			}
			return
		}
	}
	l.i++ // unreachable for valid input; skip the rune
}

func (l *lexer) word() {
	var b strings.Builder
	var subs []string
	for l.i < len(l.s) {
		r := l.s[l.i]
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || isOpStart(r) {
			break
		}
		switch r {
		case '\\':
			if l.i+1 < len(l.s) {
				b.WriteRune(l.s[l.i+1])
			}
			l.i += 2
		case '\'':
			j := l.i + 1
			for j < len(l.s) && l.s[j] != '\'' {
				j++
			}
			b.WriteString(string(l.s[l.i+1 : min(j, len(l.s))]))
			l.i = j + 1
		case '"':
			l.i++
			for l.i < len(l.s) && l.s[l.i] != '"' {
				if l.s[l.i] == '\\' && l.i+1 < len(l.s) {
					b.WriteRune(l.s[l.i+1])
					l.i += 2
					continue
				}
				if l.s[l.i] == '$' && l.i+1 < len(l.s) && l.s[l.i+1] == '(' {
					sub := l.subst()
					subs = append(subs, sub)
					b.WriteString("$(" + sub + ")")
					continue
				}
				b.WriteRune(l.s[l.i])
				l.i++
			}
			l.i++
		case '`':
			j := l.i + 1
			for j < len(l.s) && l.s[j] != '`' {
				j++
			}
			sub := string(l.s[l.i+1 : min(j, len(l.s))])
			subs = append(subs, sub)
			b.WriteString("`" + sub + "`")
			l.i = j + 1
		case '$':
			if l.i+1 < len(l.s) && l.s[l.i+1] == '(' {
				sub := l.subst()
				subs = append(subs, sub)
				b.WriteString("$(" + sub + ")")
				continue
			}
			b.WriteRune(r)
			l.i++
		default:
			b.WriteRune(r)
			l.i++
		}
	}
	l.out = append(l.out, token{kind: tWord, val: b.String(), subs: subs})
	if l.wantDelim != nil {
		l.pending = append(l.pending, struct {
			idx   int
			delim string
			tabs  bool
		}{l.wantDelim.idx, b.String(), l.wantDelim.tabs})
		l.wantDelim = nil
	}
}

// subst consumes $( ... ) at the cursor and returns its contents.
func (l *lexer) subst() string {
	l.i += 2
	start, depth := l.i, 1
	for l.i < len(l.s) && depth > 0 {
		switch l.s[l.i] {
		case '(':
			depth++
		case ')':
			depth--
		case '\'':
			for l.i++; l.i < len(l.s) && l.s[l.i] != '\''; l.i++ {
			}
		}
		l.i++
	}
	return string(l.s[start:max(start, l.i-1)])
}

func (l *lexer) readHeredocs() {
	for _, p := range l.pending {
		var body strings.Builder
		for l.i < len(l.s) {
			j := l.i
			for j < len(l.s) && l.s[j] != '\n' {
				j++
			}
			line := string(l.s[l.i:j])
			l.i = min(j+1, len(l.s))
			cmp := line
			if p.tabs {
				cmp = strings.TrimLeft(cmp, "\t")
			}
			if cmp == p.delim {
				break
			}
			body.WriteString(line + "\n")
		}
		l.out[p.idx].body = body.String()
	}
	l.pending = nil
}

type simpleCmd struct {
	words   []token
	targets []string // redirection targets
	stdin   []string // heredoc / here-string bodies
}

func split(tokens []token) []simpleCmd {
	var cmds []simpleCmd
	cur := simpleCmd{}
	flush := func() {
		if len(cur.words) > 0 || len(cur.targets) > 0 {
			cmds = append(cmds, cur)
		}
		cur = simpleCmd{}
	}
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if t.kind == tWord {
			cur.words = append(cur.words, t)
			continue
		}
		op := strings.TrimLeft(t.val, "0123456789")
		switch op {
		case ";", "\n", "&&", "||", "|", "|&", "&", "(", ")":
			flush()
		case "<<", "<<-":
			cur.stdin = append(cur.stdin, t.body)
			i++ // skip the delimiter word
		case "<<<":
			if i+1 < len(tokens) {
				cur.stdin = append(cur.stdin, tokens[i+1].val)
				i++
			}
		case "<":
			i++
		case ">&":
			if i+1 < len(tokens) {
				if v := tokens[i+1].val; v != "-" && strings.Trim(v, "0123456789") != "" {
					cur.targets = append(cur.targets, v)
				}
				i++
			}
		default: // >, >>, >|, &>, &>>, <>
			if i+1 < len(tokens) && tokens[i+1].kind == tWord {
				cur.targets = append(cur.targets, tokens[i+1].val)
				i++
			}
		}
	}
	flush()
	return cmds
}

var assignRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// words strips leading assignments and wrappers (sudo, env, command, ...).
func commandWords(ws []token) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.val)
	}
	for len(out) > 0 {
		switch {
		case assignRE.MatchString(out[0]):
			out = out[1:]
		case out[0] == "sudo" || out[0] == "env" || out[0] == "command" || out[0] == "nohup" ||
			out[0] == "time" || out[0] == "exec" || out[0] == "nice":
			out = out[1:]
			for len(out) > 0 && strings.HasPrefix(out[0], "-") {
				out = out[1:]
			}
		default:
			return out
		}
	}
	return out
}

func nonFlags(args []string) []string {
	var out []string
	for _, a := range args {
		if a == "" || (strings.HasPrefix(a, "-") && a != "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

var codeWriteRE = regexp.MustCompile(strings.Join([]string{
	`open\(\s*['"]([^'"]+)['"]\s*,\s*['"][^'"]*[wax+]`,
	`Path\(\s*['"]([^'"]+)['"]\s*\)\s*\.\s*(?:write_text|write_bytes|unlink|rename|replace|touch|mkdir|rmdir|open\(\s*['"][^'"]*[wax+])`,
	`(?:writeFile|writeFileSync|appendFile|appendFileSync|unlink|unlinkSync|rmSync|rm|rmdir|truncate|truncateSync)\(\s*['"]([^'"]+)['"]`,
	`(?:os\.remove|os\.unlink|os\.rmdir|os\.truncate|shutil\.rmtree)\(\s*['"]([^'"]+)['"]`,
	`(?:os\.rename|os\.replace|shutil\.move|shutil\.copy\w*|renameSync|copyFileSync|cpSync)\(\s*['"][^'"]+['"]\s*,\s*['"]([^'"]+)['"]`,
	`(?:os\.rename|os\.replace|shutil\.move|renameSync)\(\s*['"]([^'"]+)['"]`,
	`File\.(?:write|open|delete|unlink)\(\s*['"]([^'"]+)['"]`,
}, "|"))

// codeTargets finds literal paths that inline interpreter code writes.
func codeTargets(code string) []string {
	var out []string
	for _, m := range codeWriteRE.FindAllStringSubmatch(code, -1) {
		for _, g := range m[1:] {
			if g != "" {
				out = append(out, g)
			}
		}
	}
	return out
}

var interpreters = map[string]bool{"python": true, "python3": true, "node": true, "ruby": true, "perl": true, "deno": true, "bun": true}
var shells = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true}

// PathRef is a path a command may write, with the working directory it is
// relative to ("" when unknown, e.g. after `cd $X`).
type PathRef struct {
	Path string
	Cwd  string
}

// WriteTargets returns the paths a shell command may write. cwd is the
// directory the command starts in.
func WriteTargets(cmd, cwd string) []PathRef {
	var refs []PathRef
	add := func(p string) {
		if p != "" && p != "/dev/null" && !strings.HasPrefix(p, "/dev/") {
			refs = append(refs, PathRef{p, cwd})
		}
	}
	for _, c := range split(lex(cmd)) {
		for _, w := range c.words {
			for _, sub := range w.subs {
				refs = append(refs, WriteTargets(sub, cwd)...)
			}
		}
		for _, t := range c.targets {
			add(t)
		}
		ws := commandWords(c.words)
		if len(ws) == 0 {
			continue
		}
		name, args := path.Base(ws[0]), ws[1:]
		switch {
		case name == "cd":
			nf := nonFlags(args)
			switch {
			case len(nf) == 0 || strings.ContainsAny(nf[0], "$~`") || nf[0] == "-":
				cwd = ""
			case strings.HasPrefix(nf[0], "/"):
				cwd = path.Clean(nf[0])
			case cwd != "":
				cwd = path.Join(cwd, nf[0])
			}
		case name == "rm" || name == "rmdir" || name == "touch" || name == "truncate" || name == "mkdir" ||
			name == "unlink" || name == "shred" || name == "tee" || name == "mv" || name == "chmod" ||
			name == "chown" || name == "patch":
			for _, a := range nonFlags(args) {
				add(a)
			}
		case name == "cp" || name == "install" || name == "ln" || name == "rsync" || name == "scp":
			if nf := nonFlags(args); len(nf) > 0 {
				add(nf[len(nf)-1])
			}
		case name == "dd":
			for _, a := range args {
				if strings.HasPrefix(a, "of=") {
					add(strings.TrimPrefix(a, "of="))
				}
			}
		case name == "sed" || name == "gsed":
			inPlace := false
			for _, a := range args {
				if strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place") || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "i")) {
					inPlace = true
				}
			}
			if inPlace {
				for _, a := range nonFlags(args) {
					add(a)
				}
			}
		case name == "git" && len(args) > 0:
			switch args[0] {
			case "checkout", "restore", "rm", "mv", "clean":
				for _, a := range nonFlags(args[1:]) {
					add(a)
				}
			}
		case name == "find":
			destructive := false
			for i, a := range args {
				if a == "-delete" || ((a == "-exec" || a == "-execdir") && i+1 < len(args) && (args[i+1] == "rm" || args[i+1] == "mv")) {
					destructive = true
				}
			}
			if destructive {
				for _, a := range args {
					if strings.HasPrefix(a, "-") || a == "!" || a == "(" {
						break
					}
					add(a)
				}
			}
		case shells[name]:
			for i, a := range args {
				if a == "-c" && i+1 < len(args) {
					refs = append(refs, WriteTargets(args[i+1], cwd)...)
				}
			}
			for _, body := range c.stdin {
				refs = append(refs, WriteTargets(body, cwd)...)
			}
		case interpreters[name]:
			var code []string
			for i, a := range args {
				if (a == "-c" || a == "-e" || a == "--eval") && i+1 < len(args) {
					code = append(code, args[i+1])
				}
				if name == "perl" && strings.HasPrefix(a, "-") && strings.Contains(a, "i") && !strings.HasPrefix(a, "--") {
					for _, f := range nonFlags(args) {
						add(f)
					}
				}
			}
			code = append(code, c.stdin...)
			for _, src := range code {
				for _, p := range codeTargets(src) {
					add(p)
				}
			}
		}
	}
	return refs
}

// candidates returns the repo-relative interpretations of an unresolvable
// path: the token itself and every suffix after a "/".
func candidates(tok string) []string {
	p := Normalize(tok)
	out := []string{p}
	for i := 0; i < len(p); i++ {
		if p[i] == '/' && i+1 < len(p) {
			out = append(out, p[i+1:])
		}
	}
	return out
}

// BashViolation reports the first path a shell command writes that role may
// not write. cwd is the command's starting directory and projectDir the repo
// root (both absolute, slash-separated). Paths outside the project are
// ignored. Paths that cannot be resolved (variables, `~`) are matched by
// suffix against the protected areas, which errs on the side of blocking.
func BashViolation(role Role, cmd, cwd, projectDir string) (bool, Area, string) {
	if role == Human {
		return false, Area{}, ""
	}
	product := Areas[len(Areas)-1].Name
	projectDir = path.Clean(projectDir)
	for _, ref := range WriteTargets(cmd, cwd) {
		p := ref.Path
		unresolved := strings.ContainsAny(p, "$`") || strings.HasPrefix(p, "~") || (ref.Cwd == "" && !strings.HasPrefix(p, "/"))
		if unresolved {
			for _, c := range candidates(p) {
				if a := AreaFor(c); a.Name != product && !a.Allows(role) {
					return true, a, c
				}
			}
			continue
		}
		abs := p
		if !strings.HasPrefix(abs, "/") {
			abs = path.Join(ref.Cwd, p)
		}
		abs = path.Clean(abs)
		if abs != projectDir && !strings.HasPrefix(abs, projectDir+"/") {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(abs, projectDir), "/")
		if rel == "" {
			rel = "."
		}
		if ok, a := CanWrite(role, rel); !ok {
			return true, a, rel
		}
	}
	return false, Area{}, ""
}
