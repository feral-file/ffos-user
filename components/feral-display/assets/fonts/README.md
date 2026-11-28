# PP Mori device UI faces

Copied byte-for-byte from ff-player `public/fonts/PP_Mori` at
07805905f85d16a143bc9f409cb41a8df14c356e. These are the existing Feral File
product font assets, not an open-source font license. Preserve the product's
existing font licensing when distributing this package. Native SFNT/TTF copies
are loaded privately with Fontconfig; installation does not change system fonts.
Fontconfig can discover the WOFF originals, but Pango 1.58 on FFOS substitutes
DejaVu while shaping them. `tools/unpack-ui-fonts.py` reproduces the TTF files
with fonttools 4.61.1 and checks their glyph/metric/layout tables against the
originals. The device test verifies the actual Pango glyph-run face and width.

SHA-256:

```
4de0c27bc9df20118de4c31e5194d229fa028986dd4985c88deafe86a336553b  PPMori-Bold.woff
ec088c821ed619db6202c4106938fc7132e58553c60be768eef2905d83bc55f9  PPMori-BoldItalic.woff
b1b362d1fd3340720b933c8cc4009d122d253a1fa13bd30b170449f13acc828d  PPMori-Regular.woff
```
