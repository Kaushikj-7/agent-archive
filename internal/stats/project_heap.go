package stats

// projectHeap keeps the worst retained project at the root, so a better
// candidate replaces it without sorting every project. Name breaks every tie.
type projectHeap []Project

func (h *projectHeap) keep(row Project, limit int) {
	if limit <= 0 {
		return
	}
	if len(*h) < limit {
		*h = append(*h, row)
		for child := len(*h) - 1; child > 0; {
			parent := (child - 1) / 2
			if !projectBefore((*h)[parent], (*h)[child]) {
				break
			}
			(*h)[parent], (*h)[child] = (*h)[child], (*h)[parent]
			child = parent
		}
		return
	}
	if !projectBefore(row, (*h)[0]) {
		return
	}
	(*h)[0] = row
	h.down()
}

func (h *projectHeap) down() {
	for parent := 0; ; {
		child := parent*2 + 1
		if child >= len(*h) {
			return
		}
		if right := child + 1; right < len(*h) && projectBefore((*h)[child], (*h)[right]) {
			child = right
		}
		if !projectBefore((*h)[parent], (*h)[child]) {
			return
		}
		(*h)[parent], (*h)[child] = (*h)[child], (*h)[parent]
		parent = child
	}
}
