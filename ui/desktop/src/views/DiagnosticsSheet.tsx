// Connection diagnostics as rows with a button that copies them as text, and the service log (the
// operations the local service recorded: connects, imports, refusals) as a second page.
import { Copy, RefreshCw } from 'lucide-react';
import { useEffect, useState } from 'react';
import { Button, Loading, Note, Segmented, Sheet, Skeleton, StatusDot, cx, useUI } from '@shared/glass';
import { doctorSections, doctorText, isBadOutcome, logRows, logText, logTime, type Doctor, type LogEntry, type Tone } from '../diagnostics';
import { failureText } from '../failures';
import { request, stateLabel } from '../ipc';
import { VERSION } from '../version';

type Page = 'checks' | 'log';

const dot: Record<Tone, 'online' | 'limit' | 'pending' | 'offline'> = { ok: 'online', bad: 'limit', warn: 'pending', muted: 'offline' };
const LOG_TAIL = 300;

export function DiagnosticsSheet({ open, onClose, version }: { open: boolean; onClose: () => void; version?: string }) {
  const { copy } = useUI();
  const [page, setPage] = useState<Page>('checks');
  const [doctor, setDoctor] = useState<Doctor | null>(null);
  const [entries, setEntries] = useState<LogEntry[] | null>(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  const load = async (which: Page) => {
    setLoading(true);
    setError('');
    try {
      if (which === 'checks') setDoctor(await request<Doctor>({ type: 'doctor' }));
      else setEntries((await request<{ entries: LogEntry[] }>({ type: 'logs', tail: LOG_TAIL })).entries ?? []);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  };
  // Opened: the checks, fresh. The log is read when its page is chosen.
  useEffect(() => {
    if (!open) return;
    setPage('checks');
    setDoctor(null);
    setEntries(null);
    load('checks');
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);
  useEffect(() => {
    if (open && page === 'log' && entries === null) load('log');
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [page]);

  const sections = doctor ? doctorSections(doctor, stateLabel, failureText) : [];
  const text = page === 'checks' ? (doctor ? doctorText(doctor, version ?? VERSION, stateLabel, failureText) : '') : entries ? logText(entries) : '';
  return (
    <Sheet
      open={open}
      onClose={onClose}
      size="medium"
      title="Connection diagnostics"
      subtitle="What the local service reports about its engines, its protection checks and what it did."
      footer={
        <>
          <Button variant="ghost" icon={<RefreshCw size={14} />} disabled={loading} onClick={() => load(page)}>
            {loading ? 'Reading…' : 'Refresh'}
          </Button>
          <Button icon={<Copy size={14} />} disabled={!text} onClick={() => copy(text, page === 'checks' ? 'Report copied.' : 'Log copied.')}>
            {page === 'checks' ? 'Copy report' : 'Copy log'}
          </Button>
          <Button variant="cta" onClick={onClose}>
            Done
          </Button>
        </>
      }
    >
      <div className="grid gap-4">
        <Segmented
          size="sm"
          label="Page"
          value={page}
          onChange={setPage}
          options={[
            { value: 'checks', label: 'Checks' },
            { value: 'log', label: 'Service log' },
          ]}
        />
        {error && <Note tone="bad">{error}</Note>}
        {page === 'checks' &&
          (doctor ? (
            <div className="grid gap-4">
              {sections.map(section => (
                <div key={section.title} className="inset-glass rounded-[22px] px-4 py-3">
                  <h3 className="mb-2 text-[13.5px]">{section.title}</h3>
                  <dl className="grid grid-cols-[1fr_auto] gap-x-6 gap-y-1.5 text-[13px]">
                    {section.rows.map(row => (
                      <div key={row.label} className="contents">
                        <dt className="text-fg-3">{row.label}</dt>
                        <dd className={cx('m-0 flex items-center justify-end gap-2 text-right break-words', row.tone === 'bad' ? 'text-bad' : 'text-fg')}>
                          {row.tone !== 'muted' && <StatusDot status={dot[row.tone]} />}
                          {row.value}
                        </dd>
                      </div>
                    ))}
                  </dl>
                </div>
              ))}
              {!!doctor.limitations?.length && (
                <div className="grid gap-1.5">
                  <h3 className="text-[13.5px]">Limits</h3>
                  <ul className="grid list-disc gap-1 pl-5 text-[12.5px] text-fg-3">
                    {doctor.limitations.map(l => (
                      <li key={l}>{l}</li>
                    ))}
                  </ul>
                </div>
              )}
            </div>
          ) : (
            !error && (
              <Loading label="Running the diagnostics" className="grid gap-3">
                <Skeleton className="h-24 w-full rounded-[22px]" />
                <Skeleton className="h-28 w-full rounded-[22px]" />
              </Loading>
            )
          ))}
        {page === 'log' &&
          (entries ? (
            entries.length ? (
              <div className="inset-glass max-h-[50vh] overflow-auto rounded-[22px]">
                <table className="w-full border-collapse text-[12.5px]">
                  <thead>
                    <tr className="text-left text-[12px] text-fg-3">
                      <th className="px-3.5 py-2 font-medium">Time</th>
                      <th className="px-2 py-2 font-medium">Who</th>
                      <th className="px-2 py-2 font-medium">What</th>
                      <th className="px-3.5 py-2 font-medium">Result</th>
                    </tr>
                  </thead>
                  <tbody>
                    {logRows(entries).map((e, i) => (
                      <tr key={i} className="border-t border-line">
                        <td className="num px-3.5 py-1.5 whitespace-nowrap text-fg-2">{logTime(e.timestamp_unix)}</td>
                        <td className="px-2 py-1.5 text-fg-2">{e.actor}</td>
                        <td className="mono px-2 py-1.5">{e.operation}</td>
                        <td className={cx('px-3.5 py-1.5', isBadOutcome(e.outcome) ? 'text-bad' : 'text-fg-2')}>{e.outcome}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : (
              <Note>Nothing recorded yet.</Note>
            )
          ) : (
            !error && (
              <Loading label="Reading the service log" className="grid gap-2">
                {[0, 1, 2, 3, 4].map(i => (
                  <Skeleton key={i} className="h-5 w-full" />
                ))}
              </Loading>
            )
          ))}
        {page === 'log' && !!entries?.length && <Note>The newest {entries.length} entries. The log keeps about 1 MiB, then starts a new file. It never contains keys or passwords.</Note>}
      </div>
    </Sheet>
  );
}
