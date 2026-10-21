# subtrim

ASS subtitle cleanup.

## use

| command | behavior |
|---|---|
| `clean` | static Dialogue Text cleanup intended to preserve rendering exactly; no rendering checks or libass needed |
| `optimize` | includes `clean`, then tries rendering-validated optimizations that allow small differences |
| `compare` | renders two files and reports their differences; leaves both files untouched |

`optimize` and default `compare` allow **2/255 maximum channel error** and **0.25/255 active-pixel mean error**. optimization checks changes cumulatively against the original; exact edits require zero error. validation covers only the chosen fonts, dimensions, timestamps and renderer.

```sh
subtrim clean --output output.ass input.ass
subtrim optimize -O2 --output output.ass input.ass
subtrim compare input.ass output.ass
```

commands print a short summary. options precede input paths; quote paths containing spaces. `clean` and `optimize` require a new output path and never overwrite. use `subtrim COMMAND --help` for command-specific options.

### options

| option | `clean` | `optimize` | `compare` | meaning |
|---|:---:|:---:|:---:|---|
| `--output FILE` | ✓ | ✓ | ✓ | new output path; required for `clean`/`optimize`; optional failed-timestamp CSV for `compare`; never overwrites |
| `-O1`, `-O2` |  | ✓ |  | optimization mode; default O1 |
| `--video-size WIDTHxHEIGHT` |  | ✓ | ✓ | override the automatic video size below |
| `--downscale N` |  | ✓ | ✓ | divide video width and height by N; default `3`; `1` checks full resolution |
| `--fonts DIRECTORY` |  | ✓ | ✓ | use this folder instead of system fonts; embedded fonts are always available |
| `--libass FILE` |  | ✓ | ✓ | override the default shared-library location below |
| `--budget DURATION` |  | ✓ |  | optimization time limit, default `30s`; accepts `30s`, `2m`, etc.; `0` removes the deadline |
| `--exhaustive` |  | ✓ | ✓ | check every active integer millisecond instead of selected samples |
| `--exact` |  |  | ✓ | require zero pixel error instead of the approximation limits above |
| `--help`, `-h` | ✓ | ✓ | ✓ | show command-specific help |

**optimization modes:** `-O1` tries quick precision reductions in **Dialogue Text only**, preserving every other byte, including styles, event metadata and attachments. `-O2` searches further edits across text, styles and events while preserving **Script Info and needed embedded fonts**. it also tries removing editor-only data, unused custom sections and unneeded fonts; rendering changes or incomplete validation prevent removal.

**fonts:** omit `--fonts` to use system and embedded fonts. a supplied folder must directly contain `.ttf`, `.otf`, `.ttc` or `.otc` files; missing or empty folders are errors. style/tag font names select among available fonts; the first supported filename alphabetically provides the folder fallback.

**dimensions:** automatic video size uses a valid `LayoutResX`/`LayoutResY` pair, then `PlayResX`/`PlayResY`, then **1920×1080**. `--video-size` overrides that selection. PlayRes describes the script canvas and may differ from the actual video; supply `--video-size` when it does. libass still honors valid LayoutRes headers for its layout calculations.

`--downscale` defaults to **3 independently of ASS info**. it divides both video dimensions, rounding down to whole pixels; dimensions must remain positive. a 1920×1080 video is therefore checked at **640×360**. `--downscale 1` checks at full video resolution. the summary shows the resolved render size. downscaling changes the actual validation resolution; it does not add a full-resolution final check.

**validation:** default checks sample event boundaries, intermediate times and animation landmarks. `--exhaustive` checks every integer millisecond in event intervals plus boundary neighbours, merging overlaps and skipping inactive gaps. neither schedule has a timestamp-count cap. the time budget applies only to `optimize`; `compare` has no deadline and checks the whole selected timeline even after a mismatch, unless interrupted. exhaustive coverage does not imply zero error; `--exact` does not imply exhaustive coverage.

for zero-error comparison at every active integer millisecond, without a time limit:

```sh
subtrim compare --downscale 1 --exact --exhaustive input.ass output.ass
```

`optimize` may stop early and return its completed result; its summary states the validation coverage and whether the time budget expired. `compare` reports the total checked timestamps. on a mismatch, it also reports the number that exceeded tolerance, the first and last failing timestamps, and the worst channel and active-pixel mean errors across the whole comparison, then exits with an error. interruption or other validation failures also produce an error.

to save every failing timestamp, add `--output`:

```sh
subtrim compare --output failures.csv input.ass output.ass
```

the CSV contains `timestamp_ms`, `max_channel_error` and `active_mean_error`, with error values on the 0–255 scale. each failed timestamp has its own row, including consecutive failures. a completed comparison with no failures writes only the header. rendering mismatches still exit with an error after writing the complete file; interruption or runtime errors remove the incomplete file. without `--output`, no report file is written.

## libass runtime

`optimize` and `compare` require libass and its dependencies for the same architecture as subtrim. subtrim does not install them automatically.

| platform | library | default location |
|---|---|---|
| Windows | `libass-9.dll` | beside `subtrim.exe` |
| Linux | `libass.so.9` | system loader's search path |
| macOS | `libass.dylib` | system loader's search path |

use `--libass FILE` for another location. dependencies must be available to the system loader; on Windows, keep dependency DLLs beside libass. Windows PATH alone does not select the library.

## build

requires Go 1.23 or newer and a C compiler supported by cgo on Windows, Linux or macOS. run from the project directory:

```sh
go build -trimpath -o subtrim ./cmd/subtrim
```

use `subtrim.exe` as the output name on Windows. set `CGO_ENABLED=1` if cgo is disabled and `CC` if the compiler is not detected automatically. libass is loaded at runtime; its headers and library are not required to compile subtrim.

## acknowledgements

- https://github.com/loroid/AssPythonScripts/tree/main/AssCleanRedundantTags
- https://github.com/unanimated/luaegisub/blob/master/ua.ScriptCleanup.lua
- https://github.com/TypesettingTools/line0-Aegisub-Scripts/blob/master/l0.ASSWipe.moon
- https://github.com/TypesettingTools/ASSFoundation
- https://github.com/weizhenye/ass-compiler
- https://github.com/MontageSubs/ass-subset
