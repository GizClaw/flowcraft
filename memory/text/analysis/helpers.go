package analysis

// Terms projects structured tokens to BM25 vocabulary terms.
func Terms(tokens []Token) []string {
	if len(tokens) == 0 {
		return nil
	}
	terms := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token.Term == "" {
			continue
		}
		terms = append(terms, token.Term)
	}
	return terms
}

// UniqueTerms deduplicates token terms while preserving first-seen order.
func UniqueTerms(tokens []Token) []string {
	terms := Terms(tokens)
	if len(terms) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(terms))
	unique := make([]string, 0, len(terms))
	for _, term := range terms {
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		unique = append(unique, term)
	}
	return unique
}

// ExtractKeywords analyzes query text and returns unique BM25 query terms.
func ExtractKeywords(text string, analyzer Analyzer) []string {
	if analyzer == nil {
		return nil
	}
	return UniqueTerms(analyzer.Analyze(text, Options{Mode: ModeQuery}))
}

func recomputePositionIncrements(tokens []Token) {
	lastPosition := -1
	for i := range tokens {
		if tokens[i].PositionLength <= 0 {
			tokens[i].PositionLength = 1
		}
		switch {
		case lastPosition < 0:
			tokens[i].PositionIncr = tokens[i].Position + 1
			if tokens[i].PositionIncr <= 0 {
				tokens[i].PositionIncr = 1
			}
		case tokens[i].Position == lastPosition:
			tokens[i].PositionIncr = 0
		case tokens[i].Position > lastPosition:
			tokens[i].PositionIncr = tokens[i].Position - lastPosition
		default:
			tokens[i].PositionIncr = 1
		}
		if tokens[i].Position > lastPosition {
			lastPosition = tokens[i].Position
		}
	}
}
