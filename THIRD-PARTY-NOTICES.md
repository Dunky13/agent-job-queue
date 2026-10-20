# Third-party software

The dashboard includes a locally bundled copy of Zod 4.6.7, released under the MIT license. Its license is preserved in [ZOD-LICENSE](remote/static/vendor/ZOD-LICENSE). The bundle entry point includes its rebuild command and build dependency versions.

The Go executables include the Go runtime and the modules pinned in `go.mod` and `go.sum`, including Caddy and its certificate and authentication dependencies, modernc SQLite, Gorilla CSRF and SecureCookie, Google UUID and shlex, Go extended libraries, the plist encoder, and YAML. Caddy is licensed under Apache-2.0. The installer includes the dependencies' license and notice files in its `source/licenses` directory and preserves them in the installed support directory. Zod's license is also embedded with the dashboard assets.

GitHub Actions and JavaScript development tools are build and test dependencies; they are not embedded in the installer payload. The installed product does not require Pueue, Python, uv, a Caddy executable, or a Go toolchain. Legacy migration may inspect an existing private Pueue installation, which retains its own license.
