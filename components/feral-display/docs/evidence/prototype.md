#last-updated: 2026-11-16
# Display prototype: each work as its own page — 2026-11-16

Working session, Sean with Claude. It started from one work that would not display and ended with a prototype running on Office (FF1-NFZMNQSP, FF OS 3.0.1, staging).

## What failed

- **Work:** Leander Herzog, holz, `https://leanderherzog.ch/2026/holz/` (WebGPU, hosted by the artist).
- **What ran:** `ff-cli display "https://leanderherzog.ch/2026/holz/" -d Office` (ff-cli 4.2.1) at 13:10 PST. It exited 0 and printed "Displaying". The screen showed Chromium's gray broken-file icon.
- **Cause:** the artist's site sends `content-security-policy: frame-ancestors 'self' https://nguyenwahed.com https://www.nguyenwahed.com https://nguyenwahedgrid.cargo.site` on every page. The player shows web works in an iframe (`ArtworkSlot.tsx`, `FrameSlot`) from `http://127.0.0.1:8080/`, so Chromium refuses the frame. Same family as ff-player#302 (Art Blocks, `X-Frame-Options: sameorigin`).
- **Filed:** [comment on ff-player#302](https://github.com/feral-file/ff-player/issues/302#issuecomment-6929603826) and support report `Support [1Fdk5XrytTylH]`.
- **The work itself is fine on the device.** Opened as its own page in a new browser tab on Office, holz ran at 60 FPS.

## What Sean wants

- A work displays as its own page, at its own address, the way a browser shows it. The artist's headers are respected, never stripped or worked around.
- The Art Computer identifies the work by itself. The phone app is a controller, not a dependency: someone can plug in a Toko, set up internet, never open the app, and see Feral File's exhibition program with each work named.
- The label names the exhibition. Today a work from an exhibition is displayed with no mention of the exhibition.
- A curator's card shows only where the curator wrote one.

## The prototype

A script on Office (`walker.py`, Python standard library only) drives the kiosk browser over DevTools on `127.0.0.1:9222`. For each work it opens the work's address in a background tab, waits for it to load, brings it to the front and closes the previous tab. The player's own tab is parked on a blank page. The playlist is Living Code's 17 web works with holz added first (18 works).

Results on 11/16:

- First run: 52 changes of work, with a card before each work. No work refused, no errors.
- Second run, from 17:21: 67 changes of work by 20:51, 3 minutes each. No work refused, no errors. Memory in use on Office stayed near 1.5 to 1.7 GB of 7 GB.
- Seen on the screen or in a screenshot: holz, METASOTO, Pre-Process. For the rest, "displaying" in the log means the browser did not show an error page.

The second run shows two label forms, alternating by work, for Sean and B&F to judge:

- **Full-screen label** (odd works): a black page with exhibition name, position, artist and title for 6 seconds before the work, and again 90 seconds in. The return takes the work off the screen for those 6 seconds.
- **Label over the work** (even works): the white label bottom-left, as today, with a third line "Living Code · 2 / 18", for 30 seconds at the start and again at 90 seconds. In the prototype the script adds it to the work's own page, which is for looking only. The real one would be drawn by the device above the browser.

## What the prototype does not show

- Images and video (it has web works only), offline display, IPFS gateway routing, scaling and margins, keyboard interaction, setup screens, status back to the app.
- Whether the device's window system (`cage`) can draw a layer above the browser. A label over a running work and a cross-fade both need one. Not checked on the device.
- A run longer than a few hours.

One observation without a confirmed cause: when the player's own tab was pointed at holz, the work's web workers never started and it stayed at 0 objects. In a new tab it ran. A guess: the controller's DevTools session holds new workers paused.

## Not decided

- The label's form: full-screen between works, over the work, or both. Sean leans to both and is showing B&F.
- The cross-fade between works (asked for by Jason Ting, feral-file#3293): keep it or drop it.

## Files

`2026-11-16-display-prototype/`: `walker.py`, `card.html`, `playlist.json`.
