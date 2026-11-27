# Icon consistency

Agent Queue draws its own stacked-layers mark: a plate on two stacked layers, on a 16-point grid. `native/QueueBrand.swift` owns its geometry and renders it as a one-colour template image. The menu bar, queue header, first-launch setup, and About and updates all use that definition. Other action and status symbols retain their distinct meanings.

The menu-bar mark carries the queue state in its top plate, so the outline stays the same in every state:

| State | Top plate | When |
| --- | --- | --- |
| Idle | Outlined | Nothing is running |
| Running | Solid | At least one job runs |
| Paused | Solid, pause sign cut out | The queue is paused; this outranks a drain |
| Draining | Dashed outline | A drain closes intake |
| Attention | Solid, exclamation mark cut out | The queue is unavailable, setup is blocked, or the drain file cannot be trusted (run `agent-heavy undrain`) |

The app packages `native/Resources/AppIcon.icns` and declares it through `CFBundleIconFile`. The icon has a blue tile with the white mark in its running state. The macOS version has transparent outer padding; the menu bar uses the monochrome template for system appearance and selection. The dashboard header and README use the matching `remote/static/app-icon.png`. Browser tabs use `favicon.png`; phone home screens use the opaque `apple-touch-icon.png`, allowing the device to apply its own corner mask. The homepage uses `site/img/icon.png` and `site/favicon.png`.

Run `bash scripts/generate-icons.sh` on macOS after changing the identity. It compiles the shared definition with `native/tools/GenerateIcons.swift`, renders every required resolution with AppKit, and builds the ICNS using `iconutil`. Commit all generated assets together with the source change. Builds consume those committed files and do not regenerate them or require additional image tooling.

The remote server embeds and explicitly permits the three PNG assets under the existing authentication and security headers. The header image has empty alternative text because the adjacent heading already names the app. Its dimensions remain fixed at 52 pixels on desktop and 46 pixels on phones, in light and dark appearance.

Local validation passed through the shared queue with `bash scripts/validate.sh`: source portability, formatting, Go vet and reachable-vulnerability scanning, JavaScript and shell/workflow lint, Go race tests, seven schema tests, all 16 browser tests, native regressions, universal app and disk-image construction, isolated packaged app/controller/gateway acceptance, and read-only disk-image verification. Local execution ran on Apple Silicon; both Apple Silicon and Intel executables were built and their universal architectures verified.

Additional icon checks confirmed that both actual ZIP app bundles declare `CFBundleIconFile=AppIcon` and contain ICNS bytes identical to the source asset. Regeneration produced identical hashes for the ICNS and all three PNG files. The ICNS round-trip exposed all ten standard and Retina representations from 16 through 1024 pixels. Browser regression coverage checked image loading and dimensions at 375, 390, and 1280 pixels in light and dark appearance. Fixture screenshots at 390 and 1280 pixels in both appearances were also visually inspected.

The local build uses development signatures. These local checks do not qualify hosted CI, Intel runtime execution, Developer ID signing, notarization, publication, or installation into the real queue; those remain separate delivery gates. Existing README screenshots are historical captures and were not regenerated; the README now displays the shared icon directly. The offline HTML companion includes the complete report and embedded dashboard screenshots.
