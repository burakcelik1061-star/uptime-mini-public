'use client';

import Link from 'next/link';
import { useParams, useRouter } from 'next/navigation';
import { useCallback, useEffect, useState, type FormEvent } from 'react';

import { ImpactBadge, IncidentStatusBadge } from '@/components/IncidentBadges';
import { IncidentTimeline } from '@/components/IncidentTimeline';
import { StatusBadge } from '@/components/StatusBadge';
import { ApiError } from '@/lib/api';
import type {
  Incident,
  IncidentStatus,
  IncidentUpdate,
  Organization,
} from '@/lib/api-types';
import { formatDateTime, incidentDuration } from '@/lib/format';
import { useSession } from '@/lib/session';
import styles from './page.module.css';

/**
 * Incident detayı ve timeline (4.4).
 *
 * Timeline detay cevabının İÇİNDE geliyor (4.2): bir kesintinin anlatısı
 * olmadan detayının anlamı yok, tek ekran için ikinci bir gidiş-geliş
 * gereksiz.
 *
 * YETKİ İKİ KADEMELİ (0.1 §2): `member` timeline'a yazabiliyor (nöbetçi
 * mühendis senaryosu) ama incident'ı KAPATMAK admin+. Bu yüzden "Çözüldü"
 * seçeneği yalnızca yetkisi olana gösteriliyor — sunucu da 403 ile aynı kuralı
 * uyguluyor.
 */

const STATUS_OPTIONS: { value: IncidentStatus; label: string }[] = [
  { value: 'investigating', label: 'Araştırılıyor' },
  { value: 'identified', label: 'Sebebi bulundu' },
  { value: 'monitoring', label: 'İzleniyor' },
  { value: 'resolved', label: 'Çözüldü (incident kapanır)' },
];

export default function IncidentDetailPage() {
  const params = useParams<{ id: string }>();
  const incidentID = params.id;

  const router = useRouter();
  const { status, authFetch } = useSession();

  const [org, setOrg] = useState<Organization | null>(null);
  const [incident, setIncident] = useState<Incident | null>(null);
  const [updates, setUpdates] = useState<IncidentUpdate[]>([]);

  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (status === 'anonymous') router.replace('/login');
  }, [status, router]);

  const load = useCallback(
    async (orgID: string) => {
      const data = await authFetch<{
        incident: Incident;
        updates: IncidentUpdate[];
      }>(`/v1/orgs/${orgID}/incidents/${incidentID}`);

      setIncident(data.incident);
      setUpdates(data.updates);
    },
    [authFetch, incidentID],
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

        if (first) await load(first.id);
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
  }, [status, authFetch, load]);

  const addUpdate = useCallback(
    async (values: { status: IncidentStatus; body: string }) => {
      if (!org) return;

      await authFetch(`/v1/orgs/${org.id}/incidents/${incidentID}/updates`, {
        method: 'POST',
        body: values,
      });

      await load(org.id);
    },
    [org, authFetch, incidentID, load],
  );

  if (status !== 'authenticated') {
    return <main className={styles.page}>Yükleniyor…</main>;
  }

  const canResolve = org !== null && org.role !== 'member';
  const open = incident !== null && incident.resolvedAt === null;

  return (
    <main className={styles.page}>
      <Link className={styles.back} href="/incidents">
        ← Kesintiler
      </Link>

      {error && (
        <p className={styles.error} role="alert">
          {error}
        </p>
      )}

      {loading && !incident && <p className={styles.muted}>Yükleniyor…</p>}

      {incident && (
        <>
          <header className={styles.header}>
            <div className={styles.identity}>
              <h1 className={styles.title}>{incident.title}</h1>
              <IncidentStatusBadge status={incident.status} />
              <ImpactBadge impact={incident.impact} />
            </div>

            <dl className={styles.meta}>
              <div>
                <dt>Başlangıç</dt>
                <dd className="tabularNumbers">
                  {formatDateTime(incident.startedAt)}
                </dd>
              </div>
              <div>
                <dt>Bitiş</dt>
                <dd className="tabularNumbers">
                  {incident.resolvedAt === null
                    ? 'sürüyor'
                    : formatDateTime(incident.resolvedAt)}
                </dd>
              </div>
              <div>
                <dt>Süre</dt>
                <dd className="tabularNumbers">{incidentDuration(incident)}</dd>
              </div>
              <div>
                <dt>Kaynak</dt>
                <dd>
                  {incident.isAutoCreated
                    ? 'Otomatik (durum makinesi)'
                    : 'Manuel'}
                </dd>
              </div>
            </dl>

            <section
              className={styles.services}
              aria-label="Etkilenen servisler"
            >
              <h2 className={styles.sectionTitle}>Etkilenen servisler</h2>
              {incident.monitors.length === 0 ? (
                <p className={styles.muted}>
                  Bu incident&apos;ın kapsadığı servis kaldırılmış. Kesinti
                  kaydı geçmişte kalmaya devam ediyor.
                </p>
              ) : (
                <ul className={styles.serviceList}>
                  {incident.monitors.map((monitor) => (
                    <li key={monitor.id} className={styles.service}>
                      <Link href={`/monitors/${monitor.id}`}>
                        {monitor.name}
                      </Link>
                      <StatusBadge status={monitor.status} />
                    </li>
                  ))}
                </ul>
              )}
            </section>
          </header>

          <section aria-label="Timeline" className={styles.timelineSection}>
            <h2 className={styles.sectionTitle}>Timeline</h2>
            <IncidentTimeline updates={updates} />
          </section>

          {open ? (
            <UpdateForm canResolve={canResolve} onSubmit={addUpdate} />
          ) : (
            <p className={styles.closed}>
              Bu incident kapandı. Kapanmış bir kesintinin geçmişi geriye dönük
              değiştirilmiyor — yeni bir sorun yeni bir incident açar.
            </p>
          )}
        </>
      )}
    </main>
  );
}

/** Timeline'a güncelleme ekleme formu. */
function UpdateForm({
  canResolve,
  onSubmit,
}: {
  canResolve: boolean;
  onSubmit: (values: { status: IncidentStatus; body: string }) => Promise<void>;
}) {
  const [status, setStatus] = useState<IncidentStatus>('identified');
  const [body, setBody] = useState('');
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const options = canResolve
    ? STATUS_OPTIONS
    : STATUS_OPTIONS.filter((option) => option.value !== 'resolved');

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    setFieldErrors({});

    try {
      await onSubmit({ status, body });
      setBody('');
    } catch (cause) {
      if (cause instanceof ApiError) {
        setFieldErrors(cause.fieldMessages());
        setError(cause.message);
      } else {
        setError('Güncelleme eklenemedi');
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form className={styles.form} onSubmit={handleSubmit} noValidate>
      <h2 className={styles.sectionTitle}>Timeline&apos;a güncelleme ekle</h2>

      {error && (
        <p className={styles.error} role="alert">
          {error}
        </p>
      )}

      <div className={styles.formRow}>
        <label className={styles.field}>
          <span className={styles.label}>Durum</span>
          <select
            className={styles.input}
            value={status}
            onChange={(event) =>
              setStatus(event.target.value as IncidentStatus)
            }
          >
            {options.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
          {!canResolve && (
            <span className={styles.hint}>
              Incident&apos;ı kapatmak admin yetkisi gerektiriyor.
            </span>
          )}
        </label>

        <label className={`${styles.field} ${styles.grow}`}>
          <span className={styles.label}>Açıklama</span>
          <textarea
            className={`${styles.input} ${styles.textarea}`}
            value={body}
            onChange={(event) => setBody(event.target.value)}
            placeholder="Veritabanı bağlantı havuzu genişletildi, izliyoruz."
            required
            aria-invalid={fieldErrors.body ? true : undefined}
          />
          {fieldErrors.body && (
            <span className={styles.fieldError}>{fieldErrors.body}</span>
          )}
        </label>
      </div>

      <div className={styles.actions}>
        <button className={styles.submit} type="submit" disabled={submitting}>
          {submitting ? 'Ekleniyor…' : 'Güncelleme ekle'}
        </button>
      </div>
    </form>
  );
}
