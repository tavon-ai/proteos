package app

import (
	"flag"
	"fmt"
	"io"
)

// page is a client-side window over a list command's full result set. None of
// the control-plane's list endpoints support server-side cursors (see
// TAV-39), so --limit/--offset slice the already-fetched slice here rather
// than requesting a page from the server; Total/HasMore still let a scripted
// caller detect there's more without having to guess or re-fetch everything.
type page[T any] struct {
	Items   []T  `json:"items"`
	Total   int  `json:"total"`
	Offset  int  `json:"offset"`
	Limit   int  `json:"limit,omitempty"`
	HasMore bool `json:"has_more"`
}

// paginate windows items by offset/limit; limit<=0 means "no limit". offset
// beyond the end of items yields an empty page rather than an error.
func paginate[T any](items []T, offset, limit int) page[T] {
	total := len(items)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return page[T]{Items: items[offset:end], Total: total, Offset: offset, Limit: limit, HasMore: end < total}
}

// paginationFlags registers the --limit/--offset flags list commands share.
func paginationFlags(fs *flag.FlagSet) (limit, offset *int) {
	limit = fs.Int("limit", 0, "max items to show (0 = all)")
	offset = fs.Int("offset", 0, "items to skip before the first shown")
	return limit, offset
}

// paginated reports whether p reflects an actual --limit/--offset request, as
// opposed to the default "show everything" window.
func (p page[T]) paginated() bool {
	return p.Offset != 0 || p.Limit != 0
}

// printPageJSON emits p's items as a bare JSON array when pagination was not
// requested, preserving existing --json output for scripts that don't
// paginate; otherwise it emits the {items,total,offset,limit,has_more}
// envelope so a paginating caller can tell whether more remains.
func printPageJSON[T any](w io.Writer, p page[T]) error {
	if !p.paginated() {
		return printJSON(w, p.Items)
	}
	return printJSON(w, p)
}

// printPageFooter writes a human-readable "Showing X-Y of Z <noun>" summary to
// w, but only when pagination was actually requested — the unpaginated case
// already shows every row, so a footer would be noise.
func printPageFooter[T any](w io.Writer, p page[T], noun string) {
	if !p.paginated() {
		return
	}
	shown := len(p.Items)
	if shown == 0 {
		fmt.Fprintf(w, "\nShowing 0 of %d %s\n", p.Total, noun)
		return
	}
	from := p.Offset + 1
	to := p.Offset + shown
	fmt.Fprintf(w, "\nShowing %d-%d of %d %s", from, to, p.Total, noun)
	if p.HasMore {
		fmt.Fprintf(w, " (--offset %d for more)", to)
	}
	fmt.Fprintln(w)
}
