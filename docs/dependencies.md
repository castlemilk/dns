# Dependency policy for `apps/web`

The console is built to add no npm dependencies. Everything it needs is already
present: Connect-ES for the API, `radix-ui` and `lucide-react` for the UI,
`class-variance-authority`/`clsx`/`tailwind-merge` for styling. A screen that
seems to need a new package almost always needs a smaller component instead.

## The one accepted exception

Seven `@opentelemetry/*` packages are direct dependencies of `apps/web`:

    @opentelemetry/api
    @opentelemetry/exporter-metrics-otlp-proto
    @opentelemetry/exporter-trace-otlp-proto
    @opentelemetry/resources
    @opentelemetry/sdk-metrics
    @opentelemetry/sdk-trace-node
    @opentelemetry/semantic-conventions

They belong to the server-side observability work and are imported only from
`observability/**` and `instrumentation.node.ts`. Nothing under `app/`,
`components/`, `lib/` or `hooks/` imports them, so they never reach the browser
bundle.

## How they reach production, which is not obvious

Only `@opentelemetry/api` ships as a package in `.next/standalone/node_modules`.
The other six are **compiled into a server chunk** by Turbopack
(`.next/server/chunks/…`), so the standalone image resolves none of them at
runtime and does not need to. Verified by copying `.next/standalone` somewhere
with no parent `node_modules` and calling the built `register()` with
`NEXT_RUNTIME=nodejs`: it starts telemetry, while `require.resolve` fails for
six of the seven. Do not "fix" that resolution failure — it is the expected
shape of a bundled dependency.

## Open question for the observability workstream

`pnpm licenses:standalone` walks the shipped `node_modules` tree, so
`THIRD_PARTY.json` attributes `@opentelemetry/api` and none of the six bundled
packages. All seven are Apache-2.0, which asks for attribution wherever the code
is redistributed, and the six are redistributed inside the chunk. Either extend
`scripts/collect-standalone-licenses.mjs` to attribute bundled dependencies as
well as installed ones, or record a deliberate decision not to. This is a
compliance call, not a code defect, and it is not the console's to make.
