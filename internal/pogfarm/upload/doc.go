// Package upload posts approved clips to the target platforms.
//
// A Target is one platform (YouTube, TikTok, ...). The Loop works through
// the pending rows of the uploads table: for each enabled target it takes
// the oldest due row of an approved clip, marks it uploading, calls the
// target outside any transaction and records the result. It never posts a
// clip twice to one target; see Loop for how.
//
// Real targets live in their own packages and implement Target, and
// Checker where the platform lets them look a post up. Fake is the target
// the tests use.
package upload
