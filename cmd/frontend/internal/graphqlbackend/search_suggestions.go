package graphqlbackend

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	log15 "gopkg.in/inconshreveable/log15.v2"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/api"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/errcode"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/searchquery"

	"github.com/neelance/parallel"
	"github.com/sourcegraph/go-langserver/pkg/lsp"
)

const (
	maxSearchSuggestions = 100
)

type searchSuggestionsArgs struct {
	First *int
}

func (a *searchSuggestionsArgs) applyDefaultsAndConstraints() {
	if a.First == nil || *a.First < 0 || *a.First > maxSearchSuggestions {
		n := maxSearchSuggestions
		a.First = &n
	}
}

func (r *searchResolver) Suggestions(ctx context.Context, args *searchSuggestionsArgs) ([]*searchSuggestionResolver, error) {
	args.applyDefaultsAndConstraints()

	if len(r.query.Syntax.Expr) == 0 {
		return nil, nil
	}

	// Only suggest for type:file.
	typeValues, _ := r.query.StringValues(searchquery.FieldType)
	for _, resultType := range typeValues {
		if resultType != "file" {
			return nil, nil
		}
	}

	var suggesters []func(ctx context.Context) ([]*searchSuggestionResolver, error)

	showRepoSuggestions := func(ctx context.Context) ([]*searchSuggestionResolver, error) {
		// * If query contains only a single term (or 1 repogroup: token and a single term), treat it as a repo field here and ignore the other repo queries.
		// * If only repo fields (except 1 term in query), show repo suggestions.

		var effectiveRepoFieldValues []string
		if len(r.query.Values(searchquery.FieldDefault)) == 1 && (len(r.query.Fields) == 1 || (len(r.query.Fields) == 2 && len(r.query.Values(searchquery.FieldRepoGroup)) == 1)) {
			effectiveRepoFieldValues = append(effectiveRepoFieldValues, asString(r.query.Values(searchquery.FieldDefault)[0]))
		} else if len(r.query.Values(searchquery.FieldRepo)) > 0 && ((len(r.query.Values(searchquery.FieldRepoGroup)) > 0 && len(r.query.Fields) == 2) || (len(r.query.Values(searchquery.FieldRepoGroup)) == 0 && len(r.query.Fields) == 1)) {
			effectiveRepoFieldValues, _ = r.query.RegexpPatterns(searchquery.FieldRepo)
		}

		// If we have a query which is not valid, just ignore it since this is for a suggestion.
		i := 0
		for _, v := range effectiveRepoFieldValues {
			if _, err := regexp.Compile(v); err == nil {
				effectiveRepoFieldValues[i] = v
				i++
			}
		}
		effectiveRepoFieldValues = effectiveRepoFieldValues[:i]

		if len(effectiveRepoFieldValues) > 0 {
			_, _, repos, _, err := r.resolveRepositories(ctx, effectiveRepoFieldValues)
			return repos, err
		}
		return nil, nil
	}
	suggesters = append(suggesters, showRepoSuggestions)

	showFileSuggestions := func(ctx context.Context) ([]*searchSuggestionResolver, error) {
		// If only repos/repogroups and files are specified (and at most 1 term), then show file suggestions.
		// If the query has a file: filter AND a term, then abort; we will use showFilesWithTextMatches
		// instead.
		hasOnlyEmptyRepoField := len(r.query.Values(searchquery.FieldRepo)) > 0 && allEmptyStrings(r.query.RegexpPatterns(searchquery.FieldRepo)) && len(r.query.Fields) == 1
		hasRepoOrFileFields := len(r.query.Values(searchquery.FieldRepoGroup)) > 0 || len(r.query.Values(searchquery.FieldRepo)) > 0 || len(r.query.Values(searchquery.FieldFile)) > 0
		userQueryHasFileFilterAndTerm := len(r.query.Values(searchquery.FieldFile)) > 0 && len(r.query.Values(searchquery.FieldDefault)) > 0
		if !hasOnlyEmptyRepoField && hasRepoOrFileFields && len(r.query.Values(searchquery.FieldDefault)) <= 1 && !userQueryHasFileFilterAndTerm {
			return r.resolveFiles(ctx, maxSearchSuggestions)
		}
		return nil, nil
	}
	suggesters = append(suggesters, showFileSuggestions)

	showSymbolMatches := func(ctx context.Context) (results []*searchSuggestionResolver, err error) {

		repoRevs, _, _, _, err := r.resolveRepositories(ctx, nil)
		if err != nil {
			return nil, err
		}

		p, err := r.getPatternInfo()
		if err != nil {
			return nil, err
		}

		ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()

		fileMatches, _, err := searchSymbols(ctx, &repoSearchArgs{query: p, repos: repoRevs}, r.query, 7)
		if err != nil {
			return nil, err
		}

		results = make([]*searchSuggestionResolver, 0)
		for _, fileMatch := range fileMatches {
			for _, sr := range fileMatch.symbols {
				score := 20
				if sr.symbol.ContainerName == "" {
					score++
				}
				if len(sr.symbol.Name) < 12 {
					score++
				}
				switch sr.symbol.Kind {
				case lsp.SKFunction, lsp.SKMethod:
					score += 2
				case lsp.SKClass:
					score += 3
				}
				if len(sr.symbol.Name) >= 4 && strings.Contains(strings.ToLower(string(sr.symbol.Location.URI)), strings.ToLower(sr.symbol.Name)) {
					score++
				}
				results = append(results, newSearchResultResolver(sr, score))
			}
		}

		sort.Sort(searchResultSorter(results))
		const maxBoostedSymbolResults = 3
		boost := maxBoostedSymbolResults
		if len(results) < boost {
			boost = len(results)
		}
		if boost > 0 {
			for i := 0; i < boost; i++ {
				results[i].score += 200
			}
		}

		return results, nil
	}
	if enableSymbols {
		suggesters = append(suggesters, showSymbolMatches)
	}

	showFilesWithTextMatches := func(ctx context.Context) ([]*searchSuggestionResolver, error) {
		// If terms are specified, then show files that have text matches. Set an aggressive timeout
		// to avoid delaying repo and file suggestions for too long.
		ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if len(r.query.Values(searchquery.FieldDefault)) > 0 {
			results, err := r.doResults(ctx, "file") // only "file" result type
			if err == context.DeadlineExceeded {
				err = nil // don't log as error below
			}
			var suggestions []*searchSuggestionResolver
			if results != nil {
				if len(results.results) > *args.First {
					results.results = results.results[:*args.First]
				}
				for i, res := range results.results {
					fileResolver := &fileResolver{
						path: res.fileMatch.JPath,
						commit: &gitCommitResolver{
							oid: gitObjectID(res.fileMatch.commitID),
							// NOTE(sqs): Omits other commit fields to avoid needing to fetch them
							// (which would make it slow). This gitCommitResolver will return empty
							// values for all other fields.
							repo: &repositoryResolver{repo: res.fileMatch.repo},
						},
						stat: createFileInfo(res.fileMatch.JPath, false),
					}
					suggestions = append(suggestions, newSearchResultResolver(fileResolver, len(results.results)-i))
				}
			}
			return suggestions, err
		}
		return nil, nil
	}
	suggesters = append(suggesters, showFilesWithTextMatches)

	// Run suggesters.
	var (
		allSuggestions []*searchSuggestionResolver
		mu             sync.Mutex
		par            = parallel.NewRun(len(suggesters))
	)
	for _, suggester := range suggesters {
		par.Acquire()
		go func(suggester func(ctx context.Context) ([]*searchSuggestionResolver, error)) {
			defer par.Release()
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			suggestions, err := suggester(ctx)
			if err == nil {
				mu.Lock()
				allSuggestions = append(allSuggestions, suggestions...)
				mu.Unlock()
			} else {
				if err == context.DeadlineExceeded || err == context.Canceled {
					log15.Warn("search suggestions exceeded deadline (skipping)", "query", r.rawQuery())
				} else if !errcode.IsBadRequest(err) {
					// We exclude bad user input. Note that this means that we
					// may have some tokens in the input that are valid, but
					// typing something "bad" results in no suggestions from the
					// this suggester. In future we should just ignore the bad
					// token.
					par.Error(err)
				}
			}
		}(suggester)
	}
	if err := par.Wait(); err != nil {
		if len(allSuggestions) == 0 {
			return nil, err
		}
		// If we got partial results, only log the error and return partial results
		log15.Error("error getting search suggestions: ", "error", err)
	}

	// Eliminate duplicates.
	type key struct {
		repoURI api.RepoURI
		repoID  api.RepoID
		repoRev string
		file    string
		symbol  string
	}
	seen := make(map[key]struct{}, len(allSuggestions))
	uniqueSuggestions := allSuggestions[:0]
	for _, s := range allSuggestions {
		var k key
		switch s := s.result.(type) {
		case *repositoryResolver:
			k.repoURI = s.repo.URI
		case *fileResolver:
			k.repoID = s.commit.repositoryDatabaseID()
			k.repoRev = string(s.commit.oid)
			k.file = s.path
		case *symbolResolver:
			k.repoID = s.location.resource.commit.repoID
			k.symbol = s.symbol.Name + s.symbol.ContainerName
		default:
			panic(fmt.Sprintf("unhandled: %#v", s))
		}

		if _, dup := seen[k]; !dup {
			uniqueSuggestions = append(uniqueSuggestions, s)
			seen[k] = struct{}{}
		}
	}
	allSuggestions = uniqueSuggestions

	sort.Sort(searchResultSorter(allSuggestions))
	if len(allSuggestions) > *args.First {
		allSuggestions = allSuggestions[:*args.First]
	}

	return allSuggestions, nil
}

func allEmptyStrings(ss1, ss2 []string) bool {
	for _, s := range ss1 {
		if s != "" {
			return false
		}
	}
	for _, s := range ss2 {
		if s != "" {
			return false
		}
	}
	return true
}
