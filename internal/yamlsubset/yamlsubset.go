// Package yamlsubset reads the small part of YAML that app.yaml may use.
//
// It is written from scratch on the standard library. It accepts:
//
//   - comments and blank lines
//   - a block mapping of plain keys ("key: value")
//   - nested block mappings, by indentation with spaces
//   - a block sequence of scalars ("- value")
//   - plain scalars, and single- or double-quoted scalars with no escapes
//
// Everything else that YAML allows (anchors, aliases, tags, flow
// collections, block scalars, several documents, escapes, empty values,
// sequences of mappings) is refused with E_NEEDS_SUBTOOL. Text that is not
// YAML at all is refused with E_PARSE. Every scalar is returned as a string;
// the caller decides what a field means, so YAML's type guessing never runs.
package yamlsubset

import (
	"strings"
	"unicode/utf8"

	"github.com/ClearedStack/shipkeeper/internal/errs"
)

// Kind says what a Node holds.
type Kind int

const (
	Scalar Kind = iota
	Mapping
	Sequence
)

// Node is one value in the document.
type Node struct {
	Kind   Kind
	Line   int
	Value  string           // Scalar
	Quoted bool             // Scalar: it was written in quotes
	Keys   []string         // Mapping: keys in file order
	Fields map[string]*Node // Mapping
	Items  []*Node          // Sequence
}

type line struct {
	indent int
	text   string
	no     int
}

type parser struct {
	lines []line
	pos   int
}

// Parse reads one document.
func Parse(data []byte) (*Node, error) {
	if !utf8.Valid(data) {
		return nil, errs.YAMLNotUTF8
	}
	var p parser
	for i, raw := range strings.Split(string(data), "\n") {
		no := i + 1
		raw = strings.TrimSuffix(raw, "\r")
		indent := 0
		for indent < len(raw) && raw[indent] == ' ' {
			indent++
		}
		if indent < len(raw) && raw[indent] == '\t' {
			return nil, errs.YAMLTab.At(no)
		}
		text := strings.TrimRight(stripComment(raw[indent:]), " \t")
		if text == "" {
			continue
		}
		if indent == 0 && (strings.HasPrefix(text, "---") || strings.HasPrefix(text, "...") || text[0] == '%') {
			return nil, errs.YAMLUnsupported.At(no)
		}
		p.lines = append(p.lines, line{indent, text, no})
	}
	if len(p.lines) == 0 {
		return nil, errs.YAMLEmpty
	}
	if p.lines[0].indent != 0 {
		return nil, errs.YAMLIndent.At(p.lines[0].no)
	}
	n, err := p.node(0)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, errs.YAMLIndent.At(p.lines[p.pos].no)
	}
	return n, nil
}

// stripComment cuts a trailing comment. A '#' starts a comment at the start
// of the text or after a space, unless it is inside quotes. A quote opens
// only at the start of the text or after a space, so an apostrophe inside a
// word is just a letter.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		startOfWord := i == 0 || s[i-1] == ' '
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case (c == '"' || c == '\'') && startOfWord:
			quote = c
		case c == '#' && startOfWord:
			return s[:i]
		}
	}
	return s
}

func isItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

func (p *parser) node(indent int) (*Node, error) {
	if isItem(p.lines[p.pos].text) {
		return p.sequence(indent)
	}
	return p.mapping(indent)
}

func (p *parser) mapping(indent int) (*Node, error) {
	n := &Node{Kind: Mapping, Line: p.lines[p.pos].no, Fields: map[string]*Node{}}
	for p.pos < len(p.lines) && p.lines[p.pos].indent == indent {
		l := p.lines[p.pos]
		if isItem(l.text) {
			return nil, errs.YAMLIndent.At(l.no)
		}
		key, rest, err := splitKey(l)
		if err != nil {
			return nil, err
		}
		if _, dup := n.Fields[key]; dup {
			return nil, errs.YAMLDuplicate.At(l.no)
		}
		var child *Node
		if rest != "" {
			child, err = scalar(rest, l.no, false)
			p.pos++
		} else {
			p.pos++
			switch {
			case p.pos < len(p.lines) && p.lines[p.pos].indent > indent:
				child, err = p.node(p.lines[p.pos].indent)
			case p.pos < len(p.lines) && p.lines[p.pos].indent == indent && isItem(p.lines[p.pos].text):
				child, err = p.sequence(indent)
			default:
				err = errs.YAMLUnsupported.At(l.no) // an empty value means null
			}
		}
		if err != nil {
			return nil, err
		}
		n.Keys = append(n.Keys, key)
		n.Fields[key] = child
	}
	if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
		return nil, errs.YAMLIndent.At(p.lines[p.pos].no)
	}
	return n, nil
}

func (p *parser) sequence(indent int) (*Node, error) {
	n := &Node{Kind: Sequence, Line: p.lines[p.pos].no}
	for p.pos < len(p.lines) && p.lines[p.pos].indent == indent && isItem(p.lines[p.pos].text) {
		l := p.lines[p.pos]
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		if rest == "" {
			return nil, errs.YAMLUnsupported.At(l.no)
		}
		item, err := scalar(rest, l.no, true)
		if err != nil {
			return nil, err
		}
		n.Items = append(n.Items, item)
		p.pos++
	}
	if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
		return nil, errs.YAMLUnsupported.At(p.lines[p.pos].no) // something nested under an item
	}
	return n, nil
}

const indicators = "[{&*!|>%@`?"

func splitKey(l line) (key, rest string, err error) {
	t := l.text
	if strings.ContainsRune(indicators+"\"'", rune(t[0])) {
		return "", "", errs.YAMLUnsupported.At(l.no)
	}
	i := 0
	for i < len(t) && isKeyByte(t[i], i == 0) {
		i++
	}
	if i == 0 || i == len(t) || t[i] != ':' || (i+1 < len(t) && t[i+1] != ' ') {
		return "", "", errs.YAMLNoKey.At(l.no)
	}
	return t[:i], strings.TrimLeft(t[i+1:], " "), nil
}

func isKeyByte(c byte, first bool) bool {
	letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	if first {
		return letter
	}
	return letter || (c >= '0' && c <= '9') || c == '_' || c == '-'
}

func scalar(s string, no int, inSequence bool) (*Node, error) {
	n := &Node{Kind: Scalar, Line: no}
	switch c := s[0]; {
	case c == '"' || c == '\'':
		if len(s) < 2 || s[len(s)-1] != c {
			return nil, errs.YAMLQuote.At(no)
		}
		inner := s[1 : len(s)-1]
		if strings.IndexByte(inner, c) >= 0 {
			if c == '\'' && strings.Contains(inner, "''") {
				return nil, errs.YAMLUnsupported.At(no) // '' escape
			}
			return nil, errs.YAMLQuote.At(no)
		}
		if c == '"' && strings.IndexByte(inner, '\\') >= 0 {
			return nil, errs.YAMLUnsupported.At(no) // backslash escape
		}
		n.Value, n.Quoted = inner, true
	case strings.IndexByte(indicators, c) >= 0, isItem(s):
		return nil, errs.YAMLUnsupported.At(no)
	default:
		if strings.Contains(s, ": ") || strings.HasSuffix(s, ":") {
			if inSequence {
				return nil, errs.YAMLUnsupported.At(no) // a mapping inside a sequence
			}
			return nil, errs.YAMLScalar.At(no)
		}
		n.Value = s
	}
	return n, nil
}
