import { execFile } from 'node:child_process'
import os from 'node:os'
import path from 'node:path'
import { promisify } from 'node:util'

const execute = promisify(execFile)
const helper = path.join(os.homedir(), '.local/share/tincan/adapters/session-routing/native-context.py')

const TincanRouting = async ({ client, directory }) => ({
  'tool.execute.before': async (input, output) => {
    if (!/^(?:agent[-_]tincan_|mcp__agent_tincan__)/.test(input.tool)) return
    const sid = input.sessionID
    if (!/^ses_[A-Za-z0-9_-]+$/.test(sid || '')) throw new Error('Tincan native session identity unavailable')
    const result = await client.session.get({ path: { id: sid }, query: { directory } })
    const session = result.data
    if (session?.id !== sid || !path.isAbsolute(session.directory || '')) {
      throw new Error('Tincan native project identity unavailable')
    }
    const { stdout } = await execute('/usr/bin/python3', [helper, '--host', 'opencode',
      '--session-id', sid, '--project', session.directory], { timeout: 10000, maxBuffer: 8192 })
    output.args.host_context = stdout.trim()
  },
})

export default TincanRouting
