package tcp

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/rules"
	"github.com/traefik/traefik/v3/pkg/tcp"
	"github.com/traefik/traefik/v3/pkg/types"
	"github.com/vulcand/predicate"
)

// ConnData contains TCP connection metadata.
type ConnData struct {
	serverName string
	remoteIP   string
	alpnProtos []string
}

// NewConnData builds a connData struct from the given parameters.
func NewConnData(serverName string, conn tcp.WriteCloser, alpnProtos []string) (ConnData, error) {
	remoteIP, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return ConnData{}, fmt.Errorf("error while parsing remote address %q: %w", conn.RemoteAddr().String(), err)
	}

	// as per https://datatracker.ietf.org/doc/html/rfc6066:
	// > The hostname is represented as a byte string using ASCII encoding without a trailing dot.
	// so there is no need to trim a potential trailing dot
	serverName = types.CanonicalDomain(serverName)

	return ConnData{
		serverName: types.CanonicalDomain(serverName),
		remoteIP:   remoteIP,
		alpnProtos: alpnProtos,
	}, nil
}

// Muxer defines a muxer that handles TCP routing with rules.
type Muxer struct {
	routes   routes
	parser   predicate.Parser
	parserV2 predicate.Parser
}

// NewMuxer returns a TCP muxer.
func NewMuxer() (*Muxer, error) {
	var matcherNames []string
	for matcherName := range tcpFuncs {
		matcherNames = append(matcherNames, matcherName)
	}

	parser, err := rules.NewParser(matcherNames)
	if err != nil {
		return nil, fmt.Errorf("error while creating rules parser: %w", err)
	}

	var matchersV2 []string
	for matcher := range tcpFuncsV2 {
		matchersV2 = append(matchersV2, matcher)
	}

	parserV2, err := rules.NewParser(matchersV2)
	if err != nil {
		return nil, fmt.Errorf("error while creating v2 rules parser: %w", err)
	}

	return &Muxer{
		parser:   parser,
		parserV2: parserV2,
	}, nil
}

// Match returns the handler of the first route matching the connection metadata,
// and whether the match is exactly from the rule HostSNI(*).
func (m Muxer) Match(meta ConnData) (tcp.Handler, bool) {
	for _, route := range m.routes {
		if route.matchers.match(meta) {
			return route.handler, route.catchAll
		}
	}

	return nil, false
}

// GetRulePriority computes the priority for a given rule.
// The priority is calculated using the length of rule.
// There is a special case where the HostSNI(`*`) has a priority of -1.
func GetRulePriority(rule string) int {
	catchAllParser, err := rules.NewParser([]string{"HostSNI"})
	if err != nil {
		return len(rule)
	}

	parse, err := catchAllParser.Parse(rule)
	if err != nil {
		return len(rule)
	}

	buildTree, ok := parse.(rules.TreeBuilder)
	if !ok {
		return len(rule)
	}

	ruleTree := buildTree()

	// Special case for when the catchAll fallback is present.
	// When no user-defined priority is found, the lowest computable priority minus one is used,
	// in order to make the fallback the last to be evaluated.
	if ruleTree.RuleLeft == nil && ruleTree.RuleRight == nil && len(ruleTree.Value) == 1 &&
		ruleTree.Value[0] == "*" && strings.EqualFold(ruleTree.Matcher, "HostSNI") {
		return -1
	}

	return len(rule)
}

// AddRoute adds a new route, associated to the given handler, at the given
// priority, to the muxer.
func (m *Muxer) AddRoute(rule string, syntax string, priority int, handler tcp.Handler) error {
	var parse interface{}
	var err error
	var matcherFuncs map[string]func(*matchersTree, ...string) error

	switch syntax {
	case "v2":
		parse, err = m.parserV2.Parse(rule)
		if err != nil {
			return fmt.Errorf("error while parsing rule %s: %w", rule, err)
		}

		matcherFuncs = tcpFuncsV2
	default:
		parse, err = m.parser.Parse(rule)
		if err != nil {
			return fmt.Errorf("error while parsing rule %s: %w", rule, err)
		}

		matcherFuncs = tcpFuncs
	}

	buildTree, ok := parse.(rules.TreeBuilder)
	if !ok {
		return fmt.Errorf("error while parsing rule %s", rule)
	}

	ruleTree := buildTree()

	var matchers matchersTree
	err = matchers.addRule(ruleTree, matcherFuncs)
	if err != nil {
		return fmt.Errorf("error while adding rule %s: %w", rule, err)
	}

	var catchAll bool
	if ruleTree.RuleLeft == nil && ruleTree.RuleRight == nil && len(ruleTree.Value) == 1 {
		catchAll = ruleTree.Value[0] == "*" && strings.EqualFold(ruleTree.Matcher, "HostSNI")
	}

	newRoute := &route{
		handler:  handler,
		matchers: matchers,
		catchAll: catchAll,
		priority: priority,
	}
	m.routes = append(m.routes, newRoute)

	sort.Sort(m.routes)

	return nil
}

// HasRoutes returns whether the muxer has routes.
func (m *Muxer) HasRoutes() bool {
	return len(m.routes) > 0
}

// ParseHostSNI extracts the HostSNIs declared in a rule.
// This is a first naive implementation used in TCP routing.
func ParseHostSNI(rule string) ([]string, error) {
	var matchers []string
	for matcher := range tcpFuncs {
		matchers = append(matchers, matcher)
	}
	for matcher := range tcpFuncsV2 {
		matchers = append(matchers, matcher)
	}

	parser, err := rules.NewParser(matchers)
	if err != nil {
		return nil, err
	}

	parse, err := parser.Parse(rule)
	if err != nil {
		return nil, err
	}

	buildTree, ok := parse.(rules.TreeBuilder)
	if !ok {
		return nil, fmt.Errorf("error while parsing rule %s", rule)
	}

	return buildTree().ParseMatchers([]string{"HostSNI"}), nil
}

// routes implements sort.Interface.
type routes []*route

// Len implements sort.Interface.
func (r routes) Len() int { return len(r) }

// Swap implements sort.Interface.
func (r routes) Swap(i, j int) { r[i], r[j] = r[j], r[i] }

// Less implements sort.Interface.
func (r routes) Less(i, j int) bool { return r[i].priority > r[j].priority }

// route holds the matchers to match TCP route,
// and the handler that will serve the connection.
type route struct {
	// matchers tree structure reflecting the rule.
	matchers matchersTree
	// handler responsible for handling the route.
	handler tcp.Handler
	// catchAll indicates whether the route rule has exactly the catchAll value (HostSNI(`*`)).
	catchAll bool
	// priority is used to disambiguate between two (or more) rules that would
	// all match for a given request.
	// Computed from the matching rule length, if not user-set.
	priority int
}

// matchersTree represents the matchers tree structure.
type matchersTree struct {
	// matcher is a matcher func used to match connection properties.
	// If matcher is not nil, it means that this matcherTree is a leaf of the tree.
	// It is therefore mutually exclusive with left and right.
	matcher func(ConnData) bool
	// operator to combine the evaluation of left and right leaves.
	operator string
	// Mutually exclusive with matcher.
	left  *matchersTree
	right *matchersTree
}

func (m *matchersTree) match(meta ConnData) bool {
	if m == nil {
		// This should never happen as it should have been detected during parsing.
		log.Warn().Msg("Rule matcher is nil")
		return false
	}

	if m.matcher != nil {
		return m.matcher(meta)
	}

	switch m.operator {
	case "or":
		return m.left.match(meta) || m.right.match(meta)
	case "and":
		return m.left.match(meta) && m.right.match(meta)
	default:
		// This should never happen as it should have been detected during parsing.
		log.Warn().Str("operator", m.operator).Msg("Invalid rule operator")
		return false
	}
}

type matcherFuncs map[string]func(*matchersTree, ...string) error

func (m *matchersTree) addRule(rule *rules.Tree, funcs matcherFuncs) error {
	switch rule.Matcher {
	case "and", "or":
		m.operator = rule.Matcher
		m.left = &matchersTree{}
		err := m.left.addRule(rule.RuleLeft, funcs)
		if err != nil {
			log.WithoutContext().Warnf("\"ClientIP\" matcher: could not match remote address: %v", err)
			return false
		}
		return ok
	}

	return nil
}

// alpn checks if any of the connection ALPN protocols matches one of the matcher protocols.
func alpn(tree *matchersTree, protos ...string) error {
	if len(protos) == 0 {
		return errors.New("empty value for \"ALPN\" matcher is not allowed")
	}

	for _, proto := range protos {
		if proto == tlsalpn01.ACMETLS1Protocol {
			return fmt.Errorf("invalid protocol value for \"ALPN\" matcher, %q is not allowed", proto)
		}
	}

	tree.matcher = func(meta ConnData) bool {
		return slices.ContainsFunc(meta.alpnProtos, func(proto string) bool {
			return slices.Contains(protos, proto)
		})
	}

	return nil
}

var hostOrIP = regexp.MustCompile(`^[[:alnum:]\.\-\:]+$`)

// hostSNI checks if the SNI Host of the connection match the matcher host.
func hostSNI(tree *matchersTree, hosts ...string) error {
	if len(hosts) == 0 {
		return errors.New("empty value for \"HostSNI\" matcher is not allowed")
	}

	for i, host := range hosts {
		// Special case to allow global wildcard
		if host == "*" {
			continue
		}

		if !hostOrIP.MatchString(host) {
			return fmt.Errorf("invalid value for \"HostSNI\" matcher, %q is not a valid hostname or IP", host)
		}

		hosts[i] = strings.ToLower(host)
	}

	tree.matcher = func(meta ConnData) bool {
		// Since a HostSNI(`*`) rule has been provided as catchAll for non-TLS TCP,
		// it allows matching with an empty serverName.
		// Which is why we make sure to take that case into account before
		// checking meta.serverName.
		if hosts[0] == "*" {
			return true
		}

		if meta.serverName == "" {
			return false
		}

		for _, host := range hosts {
			if host == "*" {
				return true
			}

			if host == meta.serverName {
				return true
			}

			// trim trailing period in case of FQDN
			host = strings.TrimSuffix(host, ".")
			if host == meta.serverName {
				return true
			}
		}

		return false
	}

	return nil
}

// hostSNIRegexp checks if the SNI Host of the connection matches the matcher host regexp.
func hostSNIRegexp(tree *matchersTree, templates ...string) error {
	if len(templates) == 0 {
		return errors.New("empty value for \"HostSNIRegexp\" matcher is not allowed")
	}

	var regexps []*regexp.Regexp

	for _, template := range templates {
		preparedPattern, err := preparePattern(template)
		if err != nil {
			return err
		}

		err = funcs[rule.Matcher](m, rule.Value...)
		if err != nil {
			return err
		}

		if rule.Not {
			matcherFunc := m.matcher
			m.matcher = func(meta ConnData) bool {
				return !matcherFunc(meta)
			}
		}
	}

	return nil
}

// TODO: expose more of containous/mux fork to get rid of the following copied code (https://github.com/containous/mux/blob/8ffa4f6d063c/regexp.go).

// preparePattern builds a regexp pattern from the initial user defined expression.
// This function reuses the code dedicated to host matching of the newRouteRegexp func from the gorilla/mux library.
// https://github.com/containous/mux/tree/8ffa4f6d063c1e2b834a73be6a1515cca3992618.
func preparePattern(template string) (string, error) {
	// Check if it is well-formed.
	idxs, errBraces := braceIndices(template)
	if errBraces != nil {
		return "", errBraces
	}

	defaultPattern := "[^.]+"
	pattern := bytes.NewBufferString("")

	// Host SNI matching is case-insensitive
	_, _ = fmt.Fprint(pattern, "(?i)")

	pattern.WriteByte('^')
	var end int
	for i := 0; i < len(idxs); i += 2 {
		// Set all values we are interested in.
		raw := template[end:idxs[i]]
		end = idxs[i+1]
		parts := strings.SplitN(template[idxs[i]+1:end-1], ":", 2)
		name := parts[0]

		patt := defaultPattern
		if len(parts) == 2 {
			patt = parts[1]
		}

		// Name or pattern can't be empty.
		if name == "" || patt == "" {
			return "", fmt.Errorf("mux: missing name or pattern in %q",
				template[idxs[i]:end])
		}

		// Build the regexp pattern.
		_, _ = fmt.Fprintf(pattern, "%s(?P<%s>%s)", regexp.QuoteMeta(raw), varGroupName(i/2), patt)
	}

	// Add the remaining.
	raw := template[end:]
	pattern.WriteString(regexp.QuoteMeta(raw))
	pattern.WriteByte('$')

	return pattern.String(), nil
}

// varGroupName builds a capturing group name for the indexed variable.
// This function is a copy of varGroupName func from the gorilla/mux library.
// https://github.com/containous/mux/tree/8ffa4f6d063c1e2b834a73be6a1515cca3992618.
func varGroupName(idx int) string {
	return "v" + strconv.Itoa(idx)
}

// braceIndices returns the first level curly brace indices from a string.
// This function is a copy of braceIndices func from the gorilla/mux library.
// https://github.com/containous/mux/tree/8ffa4f6d063c1e2b834a73be6a1515cca3992618.
func braceIndices(s string) ([]int, error) {
	var level, idx int
	var idxs []int
	for i := range len(s) {
		switch s[i] {
		case '{':
			if level++; level == 1 {
				idx = i
			}
		case '}':
			if level--; level == 0 {
				idxs = append(idxs, idx, i+1)
			} else if level < 0 {
				return nil, fmt.Errorf("mux: unbalanced braces in %q", s)
			}
		}
	}
	if level != 0 {
		return nil, fmt.Errorf("mux: unbalanced braces in %q", s)
	}
	return idxs, nil
}
