package service

import "github.com/zazhedho/codex-chatgpt-bridge/internal/domain"

const defaultPageLimit = 10
const maxPageOffset = 100_000

func pageBounds(limit, offset int) (int, int, error) {
	if limit == 0 {
		limit = defaultPageLimit
	}
	if limit < 1 || limit > defaultPageLimit || offset < 0 || offset > maxPageOffset {
		return 0, 0, domain.ErrInvalidInput
	}
	return limit, offset, nil
}

func pageNextOffset(offset, limit int, hasMore bool) *int {
	if !hasMore {
		return nil
	}
	next := offset + limit
	return &next
}
