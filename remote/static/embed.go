// Package assets contains the dashboard bundled into the remote executable.
package assets

import "embed"

// Files contains only browser assets, including their vendored license.
//
//go:embed index.html app.css app.js profiles.js schema.mjs vendor/zod.mjs vendor/ZOD-LICENSE
var Files embed.FS
