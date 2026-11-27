# Title card

How the Art Computer names a work (feral-file#3615; settled by Sean and Casey on an FF1, 2026-11-24). No label is drawn over the work.

## When it shows

- A title card covers the screen in full black for 10 seconds when a playlist starts and again every hour, over whatever work is on. One work may run all day, so the card does not wait for a change of work.
- The first card of a playlist stays until the work behind it is on screen, for at most 15 more seconds. An hourly card names the work that is on screen, not one still loading.
- Changing work or sending the same playlist again keeps the hourly clock.
- The owner's label setting governs: with the label Off, no title card shows.
- A work with no title gets no card.

## What it looks like

Sizes are design pixels on a 720-high canvas, scaled by the screen's short edge, so the card holds its proportions at any resolution and rotation. Type is PP Mori.

- Margin: 50 on every side. Background `#000000`, text `#EDEDED`, quiet text `#8A8A8A`.
- Top right: the exhibition's name, which is the playlist's title ("S01:E01 · Test Exhibition #001"), 19, quiet, letter-spacing 0.02em. Left out for a lone work whose playlist title only repeats the work's; kept for a scheduled work, because the scheduler sends only today's work of an exhibition.
- Bottom left: the artist in regular and the title in bold italic, both 44 at line height 1.12, in a column at most 560 wide.
- Bottom right, when the playlist carries a link: a QR code 120 square, drawn in the text color on the black with no border, its foot on the baseline of the title's last line. "About this work" hangs 8 below it, 13, quiet, starting at the code's left edge.

## The link

The code reads the first web address (`http` or `https`) in the playlist's DP-1 `links`, of 512 characters or fewer; a longer one makes a code too dense to scan. A playlist with no link gets no code. The rule is the same for every publisher, and what page a publisher points it at is the publisher's choice.

A work's year is not a DP-1 field. A title shows a year only when the title carries it.

Code: `render_title_card` and `TitleCardClock` in `src/device_ui.cpp`; checks in `tests/device_ui_test.cpp`.
