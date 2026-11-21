package learning

import "sort"

// Reciprocal rank fusion keeps exact keyword hits competitive without comparing
// keyword relevance with a model-dependent cosine score.
func fuseRanks(semantic, lexical []string) map[string]float64 {
	scores := map[string]float64{}
	for _, list := range [][]string{semantic, lexical} {
		seen := map[string]bool{}
		for rank, id := range list {
			if !seen[id] {
				scores[id] += 1 / float64(61+rank)
				seen[id] = true
			}
		}
	}
	return scores
}

func rankByScore[T any](items []T, id func(T) string, scores map[string]float64) {
	sort.SliceStable(items, func(a, b int) bool { return scores[id(items[a])] > scores[id(items[b])] })
}
