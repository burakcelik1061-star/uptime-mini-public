'use client';

import { useRouter } from 'next/navigation';
import { useCallback, useEffect, useState } from 'react';

import { AppHeader } from '@/components/AppHeader';
import { IncidentForm } from '@/components/IncidentForm';
import { IncidentTable } from '@/components/IncidentTable';
import { ApiError } from '@/lib/api';
import type {
  CreateIncidentInput,
  Incident,
  Monitor,
  Organization,
} from '@/lib/api-types';
import { useRealtimeRefresh } from '@/lib/realtime';
import { useSession } from '@/lib/session';
import styles from './page.module.css';

/**
 * Kesinti listesi ve manuel incident oluşturma (4.4).
 *
 * SÜZGEÇ SUNUCUDA (`?status=open|resolved|all`): kapanmış incident'lar
 * zamanla birikiyor ve hepsini indirip istemcide filtrelemek, listenin
 * `limit`i yüzünden sessizce yanlış bir "açık kesinti yok" cevabı üretebilirdi
 * (2.4'ün "uptime sunucuda hesaplanır" kararının aynı gerekçesi).
 */

const FILTERS = [
  { value: 'open', label: 'Açık' },
  { value: 'resolved', label: 'Kapanmış' },
  { value: 'all', label: 'Tümü' },
] as const;

type Filter = (typeof FILTERS)[number]['value'];

export default function IncidentsPage() {
  const router = useRouter();
  const { status, authFetch } = useSession();

  const [org, setOrg] = useState<Organization | null>(null);
  const [incidents, setIncidents] = useState<Incident[]>([]);
  const [monitors, setMonitors] = useState<Monitor[]>([]);
  const [filter, setFilter] = useState<Filter>('open');
  const [creating, setCreating] = useState(false);

  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (status === 'anonymous') router.replace('/login');
  }, [status, router]);

  const loadIncidents = useCallback(
    async (orgID: string, selected: Filter) => {
      const data = await authFetch<{ incidents: Incident[] }>(
        `/v1/orgs/${orgID}/incidents?status=${selected}`,
      );
      setIncidents(data.incidents);
    },
    [authFetch],
  );

  useEffect(() => {
    if (status !== 'authenticated') return;

    let cancelled = false;

    async function load() {
      setLoading(true);
      setError(null);

      try {
        const { organizations } = await authFetch<{
          organizations: Organization[];
        }>('/v1/orgs');
        if (cancelled) return;

        const first = organizations[0] ?? null;
        setOrg(first);

        if (!first) {
          setIncidents([]);
          setMonitors([]);

          return;
        }

        // Monitor listesi manuel incident formunun servis seçimi için.
        const [, monitorData] = await Promise.all([
          loadIncidents(first.id, filter),
          authFetch<{ monitors: Monitor[] }>(`/v1/orgs/${first.id}/monitors`),
        ]);

        if (!cancelled) setMonitors(monitorData.monitors);
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

    void load();

    return () => {
      cancelled = true;
    };
  }, [status, authFetch, loadIncidents, filter]);

  // CANLI GÜNCELLEME (6.2): otomatik açılan bir kesinti listede kendiliğinden
  // beliriyor. Süzgeç de tazelemeye giriyor — açık bir listede "çözüldü"
  // olan kayıt kendiliğinden düşsün.
  useRealtimeRefresh(
    useCallback(() => {
      if (org) void loadIncidents(org.id, filter).catch(() => undefined);
    }, [org, filter, loadIncidents]),
  );

  const createIncident = useCallback(
    async (input: CreateIncidentInput) => {
      if (!org) return;

      await authFetch(`/v1/orgs/${org.id}/incidents`, {
        method: 'POST',
        body: input,
      });

      // Liste yeniden okunuyor (2.1'in kararı): sıralama ve sunucunun
      // normalleştirdiği hâl tek doğruluk kaynağı olarak kalsın.
      setCreating(false);
      setFilter('open');
      await loadIncidents(org.id, 'open');
    },
    [org, authFetch, loadIncidents],
  );

  if (status !== 'authenticated') {
    return <main className={styles.page}>Yükleniyor…</main>;
  }

  // `member` salt-okunur: incident AÇMAK admin+ (0.1 §2). Sunucu zaten 403
  // veriyor; form gösterilmiyor ki kullanıcı doldurup hata almasın.
  const canManage = org !== null && org.role !== 'member';

  return (
    <div className={styles.shell}>
      <AppHeader org={org} />

      <main className={styles.page}>
        <div className={styles.heading}>
          <h1 className={styles.title}>Kesintiler</h1>

          {canManage && (
            <button
              className={styles.action}
              type="button"
              onClick={() => setCreating((open) => !open)}
            >
              {creating ? 'Formu kapat' : 'Manuel incident aç'}
            </button>
          )}
        </div>

        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}

        {creating && canManage && (
          <IncidentForm
            monitors={monitors}
            onSubmit={createIncident}
            onCancel={() => setCreating(false)}
          />
        )}

        <nav className={styles.filters} aria-label="Kesinti süzgeci">
          {FILTERS.map((option) => (
            <button
              key={option.value}
              type="button"
              className={
                option.value === filter
                  ? `${styles.filter} ${styles.filterActive}`
                  : styles.filter
              }
              aria-pressed={option.value === filter}
              onClick={() => setFilter(option.value)}
            >
              {option.label}
            </button>
          ))}
        </nav>

        {loading ? (
          <p className={styles.muted}>Yükleniyor…</p>
        ) : (
          <IncidentTable incidents={incidents} />
        )}
      </main>
    </div>
  );
}
