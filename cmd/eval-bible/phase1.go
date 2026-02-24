package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Phase1Result holds aggregated results for the retrieval quality evaluation.
type Phase1Result struct {
	SeedsEvaluated int
	AvgCrossRefs   float64
	RecallAtK      float64
	NDCGAtK        float64
}

// seedResult holds per-seed metrics.
type seedResult struct {
	ref        string
	numXRefs   int
	recallAtK  float64
	ndcgAtK    float64
	resultRefs []string
}

// RunPhase1 evaluates retrieval quality by querying each seed verse and measuring
// how well the engine retrieves its known cross-references in the top-10.
func RunPhase1(ctx context.Context, ee *evalEngine, seeds []string, xrefs xrefMap, corpusTexts map[string]string) Phase1Result {
	if len(seeds) == 0 {
		return Phase1Result{}
	}

	results := make([]seedResult, 0, len(seeds))
	var totalRecall, totalNDCG float64
	var totalXRefs int

	for i, seed := range seeds {
		text, ok := corpusTexts[seed]
		if !ok {
			continue
		}

		// Query the engine using the verse text as context
		activations, err := ee.activate(ctx, []string{seed + " " + text})
		if err != nil {
			fmt.Printf("  [error] %s: %v\n", seed, err)
			continue
		}

		// Build result list from activation concept fields
		resultRefs := make([]string, len(activations))
		for j, act := range activations {
			resultRefs[j] = act.Concept
		}

		// Build relevant set from known cross-references
		known := xrefs[seed]
		relevant := make(map[string]bool, len(known))
		for _, ref := range known {
			relevant[ref] = true
		}

		// Filter to only NT refs if seed is NT
		if len(relevant) == 0 {
			// No cross-refs — skip this seed
			continue
		}

		recall := recallAtK(resultRefs, relevant, 10)
		ndcg := ndcgAtK(resultRefs, relevant, 10)

		sr := seedResult{
			ref:        seed,
			numXRefs:   len(known),
			recallAtK:  recall,
			ndcgAtK:    ndcg,
			resultRefs: resultRefs,
		}
		results = append(results, sr)
		totalRecall += recall
		totalNDCG += ndcg
		totalXRefs += len(known)

		if (i+1)%10 == 0 {
			fmt.Printf("  Phase 1: %d/%d seeds evaluated...\n", i+1, len(seeds))
		}
	}

	if len(results) == 0 {
		return Phase1Result{SeedsEvaluated: 0}
	}

	n := float64(len(results))
	return Phase1Result{
		SeedsEvaluated: len(results),
		AvgCrossRefs:   float64(totalXRefs) / n,
		RecallAtK:      totalRecall / n,
		NDCGAtK:        totalNDCG / n,
	}
}

// buildCorpusTextMap builds a map from concept (verse ref) to verse content.
func buildCorpusTextMap(reqs []mbp.WriteRequest) map[string]string {
	m := make(map[string]string, len(reqs))
	for _, r := range reqs {
		m[r.Concept] = r.Content
	}
	return m
}

// ntBookNames is the set of New Testament book names for NT reference detection.
var ntBookNames = map[string]bool{
	"Matthew": true, "Mark": true, "Luke": true, "John": true,
	"Acts": true, "Romans": true, "1 Corinthians": true, "2 Corinthians": true,
	"Galatians": true, "Ephesians": true, "Philippians": true, "Colossians": true,
	"1 Thessalonians": true, "2 Thessalonians": true, "1 Timothy": true, "2 Timothy": true,
	"Titus": true, "Philemon": true, "Hebrews": true, "James": true,
	"1 Peter": true, "2 Peter": true, "1 John": true, "2 John": true,
	"3 John": true, "Jude": true, "Revelation": true,
}

// isNTRef returns true if the given reference begins with a New Testament book name.
func isNTRef(ref string) bool {
	// ref format: "BookName chapter:verse"
	// Extract book name by splitting on space and reconstructing until digit
	parts := strings.Fields(ref)
	if len(parts) == 0 {
		return false
	}
	// Book name may be multi-word (e.g. "1 Corinthians"); last field has "chapter:verse"
	// Collect all parts that are not "chapter:verse"
	var bookParts []string
	for _, p := range parts {
		if strings.Contains(p, ":") {
			break
		}
		bookParts = append(bookParts, p)
	}
	bookName := strings.Join(bookParts, " ")
	return ntBookNames[bookName]
}
