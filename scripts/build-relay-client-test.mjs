import { mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawnSync } from 'node:child_process'

const root = dirname(dirname(fileURLToPath(import.meta.url)))
const server = join(root, 'server')
const output = join(root, 'dist', 'relay-client-test')
mkdirSync(output, { recursive: true })

const targets = [
  { goos: 'windows', goarch: 'amd64', name: 'relay-client-test-windows-amd64.exe' },
  { goos: 'linux', goarch: 'amd64', name: 'relay-client-test-linux-amd64' },
  { goos: 'darwin', goarch: 'arm64', name: 'relay-client-test-darwin-arm64' },
]

for (const target of targets) {
  const destination = join(output, target.name)
  const result = spawnSync(
    'go',
    [
      'build',
      '-buildvcs=false',
      '-trimpath',
      '-ldflags=-s -w -X main.version=0.1.0',
      '-o',
      destination,
      './cmd/relay-client-test',
    ],
    {
      cwd: server,
      env: { ...process.env, GOOS: target.goos, GOARCH: target.goarch, CGO_ENABLED: '0' },
      encoding: 'utf8',
      stdio: 'inherit',
    },
  )
  if (result.error) throw result.error
  if (result.status !== 0) process.exit(result.status ?? 1)
  process.stdout.write(`built ${destination}\n`)
}
