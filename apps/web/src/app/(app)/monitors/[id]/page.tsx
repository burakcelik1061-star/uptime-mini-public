'use client';

import Link from 'next/link';
import { useParams, useRouter } from 'next/navigation';
import { useCallback, useEffect, useState } from 'react';

import { CheckTable } from '@/components/CheckTable';
import { MonitorForm, type MonitorFormValues } from '@/components/MonitorForm';
import { ResponseTimeChart } from '@/components/ResponseTimeChart';
import { StatusBadge } from '@/components/StatusBadge';
import { ApiError } from '@/lib/api';
import type {
  CheckResult,
  CheckSummary,
  Monitor,
  Organization,
} from '@/lib/api-types';
import { useSession } from '@/lib/session';
import styles from './page.module.css';

/**
 * Monitor detay sayfası (2.4): mevcut durum, seçilen aralık için ham uptime
 * yüzdesi, yanıt süresi grafiği ve son kontroller.
 *
 * 2.1'in ertelediği eylemler de burada: "şimdi kontrol et", duraklat/devam,
 * düzenle, sil. Listede değil BURADA olmalarının sebebi 2.1'de yazılmıştı —
 * bir satırın ne yaptığını göstermeden yanına "sil" koymak listeyi kaza
 * yüzeyine çevirir.
 */

/** Aralık seçenekleri; değerler SAAT cinsinden. */
const RANGES = [
  { hours: 1, label: '1 saat' },
  { hours: 24, label: '24 saat' },
  { hours: 24 * 7, label: '7 gün' },
] as const;

/** Grafik ve liste tek istekte en fazla bu kadar kontrol okuyor (uç sınırı). */
const CHECK_LIMIT = 200;

export default function MonitorDetailPage() {
  const params = useParams<{ id: string }>();
  const monitorID = params.id;

  const router = useRouter();
  const { status, authFetch } = useSession();

  const [org, setOrg] = useState<Organization | null>(null);
  const [monitor, setMonitor] = useState<Monitor | null>(null);
  const [summary, setSummary] = useState<CheckSummary | null>(null);
  const [checks, setChecks] = useState<CheckResult[]>([]);
  const [rangeHours, setRangeHours] = useState<number>(24);

  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [confirmingDelete, setConfirmingDelete] = useState(false);

  useEffect(() => {
    if (status === 'anonymous') router.replace('/login');
  }, [status, router]);

  /** load, monitor + özet + kontrolleri seçilen aralık için okur. */
  const load = useCallback(
    async (orgID: string, hours: number) => {
      const to = new Date();
      const from = new Date(to.getTime() - hours * 60 * 60 * 1000);

      const range = `from=${encodeURIComponent(from.toISOString())}&to=${encodeURIComponent(
        to.toISOString(),
      )}`;
      const base = `/v1/orgs/${orgID}/monitors/${monitorID}`;

      const [monitorData, summaryData, checksData] = await Promise.all([
        authFetch<{ monitor: Monitor }>(base),
        authFetch<{ summary: CheckSummary }>(`${base}/summary?${range}`),
        authFetch<{ checks: CheckResult[] }>(
          `${base}/checks?${range}&limit=${CHECK_LIMIT}`,
        ),
      ]);

      setMonitor(monitorData.monitor);
      setSummary(summaryData.summary);
      setChecks(checksData.checks);
    },
    [authFetch, monitorID],
  );

  useEffect(() => {
    if (status !== 'authenticated') return;

    let cancelled = false;

    async function loadAll() {
      setLoading(true);
      setError(null);

      try {
        const { organizations } = await authFetch<{
          organizations: Organization[];
        }>('/v1/orgs');
        if (cancelled) return;

        const first = organizations[0] ?? null;
        setOrg(first);

        if (first) await load(first.id, rangeHours);
      } catch (cause) {
        if (!cancelled) {
          setError(
            cause instanceof ApiError ? cause.message : 'Veriler yüklenemedi',
          );
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    }

    void loadAll();

    return () => {
      cancelled = true;
    };
  }, [status, authFetch, load, rangeHours]);

  /** run, bir eylemi çalıştırıp veriyi tazeler. */
  const run = useCallback(
    async (label: string, action: () => Promise<void>) => {
      if (!org) return;

      setBusy(label);
      setError(null);

      try {
        await action();
        await load(org.id, rangeHours);
      } catch (cause) {
        setError(cause instanceof ApiError ? cause.message : 'İşlem başarısız');
      } finally {
        setBusy(null);
      }
    },
    [org, load, rangeHours],
  );

  const runCheck = () =>
    run('check', async () => {
      await authFetch(`/v1/orgs/${org?.id}/monitors/${monitorID}/check`, {
        method: 'POST',
      });
    });

  const toggleActive = () =>
    run('toggle', async () => {
      await authFetch(`/v1/orgs/${org?.id}/monitors/${monitorID}`, {
        method: 'PATCH',
        body: { isActive: !monitor?.isActive },
      });
    });

  const saveEdit = async (values: MonitorFormValues) => {
    if (!org) return;

    await authFetch(`/v1/orgs/${org.id}/monitors/${monitorID}`, {
      method: 'PATCH',
      body: values,
    });

    setEditing(false);
    await load(org.id, rangeHours);
  };

  const remove = async () => {
    if (!org) return;

    setBusy('delete');

    try {
      await authFetch(`/v1/orgs/${org.id}/monitors/${monitorID}`, {
        method: 'DELETE',
      });
      router.replace('/');
    } catch (cause) {
      setError(cause instanceof ApiError ? cause.message : 'Silinemedi');
      setBusy(null);
    }
  };

  if (status !== 'authenticated') {
    return <main className={styles.page}>Yükleniyor…</main>;
  }

  const canEdit = org !== null && org.role !== 'member';

  return (
    <main className={styles.page}>
      <Link className={styles.back} href="/">
        ← Monitörler
      </Link>

      {error && (
        <p className={styles.error} role="alert">
          {error}
        </p>
      )}

      {loading && !monitor && <p className={styles.muted}>Yükleniyor…</p>}

      {monitor && (
        <>
          <header className={styles.header}>
            <div className={styles.identity}>
              <h1 className={styles.title}>{monitor.name}</h1>
              <StatusBadge status={monitor.status} />
            </div>

            <a
              className={styles.url}
              href={monitor.url}
              target="_blank"
              rel="noreferrer noopener"
            >
              {monitor.url}
            </a>

            <dl className={styles.meta}>
              <div>
                <dt>Metot</dt>
                <dd>{monitor.method}</dd>
              </div>
              <div>
                <dt>Aralık</dt>
                <dd className="tabularNumbers">
                  {monitor.checkIntervalSeconds} sn
                </dd>
              </div>
              <div>
                <dt>Zaman aşımı</dt>
                <dd className="tabularNumbers">{monitor.timeoutMs} ms</dd>
              </div>
              <div>
                <dt>Beklenen status</dt>
                <dd className="tabularNumbers">
                  {monitor.expectedStatusMin}–{monitor.expectedStatusMax}
                </dd>
              </div>
              <div>
                <dt>“Yavaş” eşiği</dt>
                <dd className="tabularNumbers">
                  {monitor.degradedResponseMs === null
                    ? 'kapalı'
                    : `${monitor.degradedResponseMs} ms`}
                </dd>
              </div>
            </dl>
          </header>

          {canEdit && (
            <div className={styles.actions}>
              <button
                className={styles.primaryAction}
                type="button"
                onClick={() => void runCheck()}
                disabled={busy !== null}
              >
                {busy === 'check' ? 'Kontrol ediliyor…' : 'Şimdi kontrol et'}
              </button>

              <button
                className={styles.action}
                type="button"
                onClick={() => void toggleActive()}
                disabled={busy !== null}
              >
                {monitor.isActive ? 'Duraklat' : 'Devam ettir'}
              </button>

              <button
                className={styles.action}
                type="button"
                onClick={() => setEditing((open) => !open)}
                disabled={busy !== null}
              >
                {editing ? 'Düzenlemeyi kapat' : 'Düzenle'}
              </button>

              {/* Silme iki adımlı: tek tıkla geri alınamaz bir işlem
                  yapılmıyor. */}
              {confirmingDelete ? (
                <span className={styles.confirm}>
                  Bu monitor ve tüm kontrol geçmişi silinecek.
                  <button
                    className={styles.danger}
                    type="button"
                    onClick={() => void remove()}
                    disabled={busy !== null}
                  >
                    Evet, sil
                  </button>
                  <button
                    className={styles.action}
                    type="button"
                    onClick={() => setConfirmingDelete(false)}
                  >
                    Vazgeç
                  </button>
                </span>
              ) : (
                <button
                  className={styles.action}
                  type="button"
                  onClick={() => setConfirmingDelete(true)}
                  disabled={busy !== null}
                >
                  Sil
                </button>
              )}
            </div>
          )}

          {editing && canEdit && (
            <MonitorForm
              initial={monitor}
              title="Monitor'ü düzenle"
              submitLabel="Kaydet"
              onSubmit={saveEdit}
              onCancel={() => setEditing(false)}
            />
          )}

          <nav className={styles.ranges} aria-label="Zaman aralığı">
            {RANGES.map((range) => (
              <button
                key={range.hours}
                type="button"
                className={
                  range.hours === rangeHours
                    ? `${styles.range} ${styles.rangeActive}`
                    : styles.range
                }
                aria-pressed={range.hours === rangeHours}
                onClick={() => setRangeHours(range.hours)}
              >
                {range.label}
              </button>
            ))}
          </nav>

          {summary && <SummaryCards summary={summary} />}

          <ResponseTimeChart checks={checks} />

          {checks.length === CHECK_LIMIT && (
            <p className={styles.note}>
              Bu aralıkta en fazla {CHECK_LIMIT} kontrol gösteriliyor; uptime
              yüzdesi aralığın TAMAMINDAN hesaplanıyor.
            </p>
          )}

          <CheckTable checks={checks} />
        </>
      )}
    </main>
  );
}

/**
 * Özet kartları.
 *
 * Uptime `null` olduğunda "%100" değil "—" gösteriliyor: veri yokluğu ile
 * kesintisizlik aynı şey değil (0.1'in bağlayıcı kararı).
 */
function SummaryCards({ summary }: { summary: CheckSummary }) {
  const uptime =
    summary.uptimePercent === null
      ? '—'
      : `%${summary.uptimePercent.toFixed(2)}`;

  const average =
    summary.avgResponseTimeMs === null
      ? '—'
      : `${Math.round(summary.avgResponseTimeMs)} ms`;

  return (
    <section className={styles.cards} aria-label="Aralık özeti">
      <article className={styles.card}>
        <h2 className={styles.cardTitle}>Uptime</h2>
        <p className={`${styles.cardValue} tabularNumbers`}>{uptime}</p>
        <p className={styles.cardNote}>
          {summary.scheduledChecks === 0
            ? 'Zamanlanmış kontrol yok (Faz 3 ile gelecek)'
            : `${summary.successChecks} başarılı · ${summary.degradedChecks} yavaş · ${summary.failureChecks} başarısız`}
        </p>
      </article>

      <article className={styles.card}>
        <h2 className={styles.cardTitle}>Kontrol</h2>
        <p className={`${styles.cardValue} tabularNumbers`}>
          {summary.scheduledChecks}
        </p>
        <p className={styles.cardNote}>
          {/* Uptime'a GİRMEYEN iki kategori ayrı ayrı yazılıyor: manuel
              kontroller (2.3) ve planlı bakım penceresine düşenler (4.3).
              İkincisi paydadan düşüldüğü için yüzdeyi doğrudan etkiliyor —
              görünmez olması "neden %100 değil / neden %100" sorusunu
              cevapsız bırakırdı. */}
          {[
            summary.maintenanceChecks > 0
              ? `${summary.maintenanceChecks} planlı bakım`
              : null,
            summary.manualChecks > 0 ? `${summary.manualChecks} manuel` : null,
          ]
            .filter((note) => note !== null)
            .join(' · ') || 'zamanlanmış kontrol'}
          {(summary.maintenanceChecks > 0 || summary.manualChecks > 0) &&
            " (uptime'a girmez)"}
        </p>
      </article>

      <article className={styles.card}>
        <h2 className={styles.cardTitle}>Ortalama yanıt</h2>
        <p className={`${styles.cardValue} tabularNumbers`}>{average}</p>
        <p className={styles.cardNote}>ölçümü olan kontrollerden</p>
      </article>
    </section>
  );
}
