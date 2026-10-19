# Third-party software

The dashboard includes a locally bundled copy of Zod 4.6.7, released under the MIT license. Its license is preserved in [ZOD-LICENSE](remote/static/vendor/ZOD-LICENSE). The bundle entry point includes its rebuild command and build dependency versions.

Pueue, Caddy, Python, uv, Flask, Flask-WTF, Waitress, and their dependencies are installed separately. They retain their own licenses. GitHub Actions and JavaScript development tools are build and test dependencies; they are not embedded in the installer payload.
