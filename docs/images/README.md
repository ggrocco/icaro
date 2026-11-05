# README screenshots

These are captures of Ícaro running against a disposable local daemon with demo workflows. They show actual application data, without provider credentials or mocked API responses.

| File | What it shows | Capture environment |
| --- | --- | --- |
| [web-run.png](web-run.png) | The successful `hello` run: connected steps, the `greeting` output, and the `greet` log | Chromium on Linux, light theme, 1360 px viewport |
| [desktop-app.png](desktop-app.png) | The native desktop app's workflow form editor, validation status, and graph | Linux source build with Wails, GTK 4, and WebKitGTK 6, on a virtual display |

The desktop image shows the Linux source build. The macOS app uses the same web UI, with its own native window and menu bar integration. This capture does not establish macOS installation, signing, or notification behavior; see the [desktop status](../desktop.md).

## Refresh the images

1. Follow [CONTRIBUTING.md](../../CONTRIBUTING.md) to build the current UI and CLI with `make web build`. Use a separate `ICARO_HOME` for demo state so personal workflows and credentials do not appear.
2. Initialize the demo home and start its daemon with `icaro init` and `icaro serve --background`. From the repository root, apply and run the examples:

   ```sh
   icaro workflow apply examples/hello.yaml
   icaro run hello --set who=icaro --wait
   icaro workflow apply examples/mcp-hello.yaml
   icaro run mcp-hello --set who=icaro --wait
   ```

3. Run `icaro ui`, open the successful `hello` run, and select **greet**. Wait for the graph and logs to load. Capture the page at a 1360 px viewport width, including the output and log panel, as `web-run.png`.
4. Build and launch the native app using the [desktop build instructions](../desktop.md#building-it), with the same `ICARO_HOME`. Open **hello → Edit → Form** and capture the app window, including its native title bar, as `desktop-app.png`. On a headless Linux machine, use a virtual display such as Xvfb and a window manager.
5. Keep the UI unchanged during capture. Do not include API tokens, one-time sign-in links, personal paths, or real customer data. Use PNG and retain legible text at the README's display width.
6. Update the captions and capture-environment table if the view or platform changes. Run `go run ./hack/linkcheck .` from the repository root, then stop the demo daemon and remove its disposable state.
