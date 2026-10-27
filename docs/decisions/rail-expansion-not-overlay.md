
## What

Opening the Thread Explorer **expands the left rail**. It is not a route, a
pane swap, or a floating panel: the same `<aside class="sidebar">` grows to
`max-content` over the workspace, the compact thread list is swapped for the
table, and the live session keeps rendering behind it.

Selecting a historical thread is not a dialog: the rail collapses and its
read-only transcript and evidence view occupies the main workspace. Closing
that history view returns to the already-selected live session.

Four invariants make the expansion read as one surface rather than two. Each
was arrived at by measuring the running app; each is easy to break silently
from either side.

1. **One ordering.** `explorer.view.sort` is the only sort. `projectSidebar`
   **does not sort** — it preserves the order of the dataset it is handed, and
   `App.svelte` hands it `applyView(explorer.dataset, explorer.view)`, the same
   pipeline the table uses. The sort toggle and the column headers write the
   same field, so an ordering set on either surface is already in force when
   the other appears. Re-introducing a sort inside `projectSidebar` silently
   re-splits the two views.

2. **One row geometry.** A thread occupies the identical x and y in both
   states. This requires the list gutters to match (`.xlist` and
   `.thread-list` both 6px), the row pitch to equal `--row-h` exactly (no
   margin on `.thread-row`), the title to be 13px in both, and the header
   stack above the list to be the same three bands — brand 47px, header 38px,
   sort 33px, then chips 26px + 6px list padding = the column header's 32px.
   Adding a band to one state without the other moves every thread.

3. **One density.** `--row-h` on `.shell` drives both renderers: full density
   is title → working directory → model · effort; compact keeps title →
   working directory. The right-aligned updated age is a separate color-coded
   channel (fresh/warm/stale), never a replacement for row metadata. The
   `full`/`compact` toggle is a persisted user preference, not view state.
   Provider is represented by the Explorer's dedicated Provider column and
   never repeated in either row title.

4. **Nothing already on screen animates.** The reveal is a `clip-path` wipe
   anchored at the rail's right edge, so the first 230px never move or fade.
   Both bodies stay mounted and swap with `display: none`.

## Why

The rail and the table render the same catalog. Anything that makes them *look* like different windows —
different order, different row heights, a fade of already-visible pixels, a
4px gutter difference — reads as "a second window opened", which is the thing
this shape exists to avoid.

Two implementation traps found by measurement, worth not rediscovering:

- **Animating width reflows the table.** A width transition re-lays-out the
  11-column grid at every intermediate width, crushing and unfolding the
  columns. Only `clip-path`/`transform`/`opacity` are safe here, because the
  panel's width is fixed by its content.
- **`content-visibility: auto` breaks the rows.** It would cut the reveal from
  ~126ms to ~59ms, but `.xrow` uses `grid-template-columns: subgrid`, and the
  containment it implies drops rows out of the parent grid — every cell
  collapses to the same x. Rejected for that reason.

## Cost

Mounting both bodies costs the Explorer's rows at startup rather than on click
(~370ms of dead time per open, otherwise). The working directory is carried in
the shared name cell as the stable middle line; the sortable/facetable Provider
column is the only provider label in the expanded table.
