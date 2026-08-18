'use client';

import { useRouter } from 'next/navigation';
import { useCallback, useEffect, useState, type FormEvent } from 'react';

import { AppHeader } from '@/components/AppHeader';
import { MonitorForm, type MonitorFormValues } from '@/components/MonitorForm';
import { MonitorTable } from '@/components/MonitorTable';
import { ApiError } from '@/lib/api';
import type { Monitor, Organization } from '@/lib/api-types';
import { useRealtimeRefresh } from '@/lib/realtime';
import { useSession } from '@/lib/session';
import styles from './page.module.css';

/**
 * Yönetim panosu — monitor listesi ve ekleme formu (Faz 2.1).
 *
 * ORGANİZASYON SEÇİMİ: kullanıcının ilk organizasyonu kullanılıyor. Birden
 * fazla organizasyonu olan kullanıcı için seçici sonraki bir adımda gelecek;
 * bugün üyelik ekleme akışı (davet) olmadığı için bir kullanıcının birden
 * fazla organizasyonu ancak kendi açtıklarıyla oluşuyor.
 */
export default function DashboardPage() {
  const router = useRouter();
  const { status, authFetch } = useSession();

  const [org, setOrg] = useState<Organization | null>(null);
  const [monitors, setMonitors] = useState<Monitor[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (status === 'anonymous') router.replace('/login');
  }, [status, router]);

  const loadMonitors = useCallback(
    async (orgID: string) => {
      const data = await authFetch<{ monitors: Monitor[] }>(
        `/v1/orgs/${orgID}/monitors`,
      );
      setMonitors(data.monitors);
    },
    [authFetch],
  );

  useEffect(() => {
    if (status !== 'authenticated') return;

    // İstek uçuşta iken kullanıcı çıkış yaparsa (veya bileşen sökülürse)
    // gelen cevabın state'e yazılmaması gerekiyor.
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

        if (first) {
          await loadMonitors(first.id);
        } else {
          setMonitors([]);
        }
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
  }, [status, authFetch, loadMonitors]);

  // CANLI GÜNCELLEME (6.2): olay gövdesiz bir "değişti" sinyali, cevabı da
  // her zaman aynı — listeyi yeniden oku. Böylece canlı yayının söylediğiyle
  // panonun gösterdiği ayrışamıyor ve kopukken kaçırılan olaylar ayrı bir
  // mekanizma gerektirmiyor (yeniden bağlanmanın kendisi bir tazeleme sebebi).
  useRealtimeRefresh(
    useCallback(() => {
      if (org) void loadMonitors(org.id).catch(() => undefined);
    }, [org, loadMonitors]),
  );

  const createMonitor = useCallback(
    async (input: MonitorFormValues) => {
      if (!org) return;

      // Cevaptaki kaydı listeye eklemek yerine liste yeniden okunuyor:
      // sunucunun normalleştirdiği hâl (URL, varsayılanlar) ve sıralama tek
      // doğruluk kaynağı olarak kalsın.
      await authFetch<{ monitor: Monitor }>(`/v1/orgs/${org.id}/monitors`, {
        method: 'POST',
        body: input,
      });

      await loadMonitors(org.id);
    },
    [org, authFetch, loadMonitors],
  );

  const createOrganization = useCallback(
    async (name: string) => {
      const { organization } = await authFetch<{
        organization: Organization;
      }>('/v1/orgs', { method: 'POST', body: { name } });

      setOrg(organization);
      setMonitors([]);
    },
    [authFetch],
  );

  if (status !== 'authenticated') {
    return <main className={styles.page}>Yükleniyor…</main>;
  }

  // `member` rolü salt-okunur (0.1 §2); sunucu da POST'u 403 ile
  // reddediyor. Form gösterilmiyor ki kullanıcı doldurup hata almasın.
  const canEdit = org !== null && org.role !== 'member';

  return (
    <div className={styles.shell}>
      {/* Üst çubuk 4.4'te ORTAK bileşene taşındı: incident ve bakım
          ekranlarıyla üç kopya olmasın. */}
      <AppHeader org={org} />

      <main className={styles.page}>
        <h1 className={styles.title}>Monitörler</h1>

        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}

        {loading && <p className={styles.muted}>Yükleniyor…</p>}

        {!loading && !org && (
          <CreateOrganization onCreate={createOrganization} />
        )}

        {!loading && org && (
          <>
            {canEdit ? (
              <MonitorForm onSubmit={createMonitor} />
            ) : (
              <p className={styles.muted}>
                Bu organizasyondaki rolünüz salt-okunur; monitor ekleme yetkisi
                admin ve üzerindedir.
              </p>
            )}

            <MonitorTable monitors={monitors} />
          </>
        )}
      </main>
    </div>
  );
}

/**
 * Hiç organizasyonu olmayan kullanıcı için çıkış yolu.
 *
 * Olmasaydı yeni kaydolan bir kullanıcı boş bir panoda kalırdı: monitor
 * uçlarının tamamı bir organizasyona bağlı ve organizasyon oluşturmanın
 * arayüzü henüz yok.
 */
function CreateOrganization({
  onCreate,
}: {
  onCreate: (name: string) => Promise<void>;
}) {
  const [name, setName] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);

    try {
      await onCreate(name);
    } catch (cause) {
      setError(
        cause instanceof ApiError ? cause.message : 'Organizasyon açılamadı',
      );
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form className={styles.orgCard} onSubmit={handleSubmit} noValidate>
      <h2 className={styles.orgTitle}>Önce bir organizasyon açın</h2>
      <p className={styles.muted}>
        Monitörler bir organizasyona ait olur; adres adı (slug) sunucu
        tarafından üretilir.
      </p>

      {error && (
        <p className={styles.error} role="alert">
          {error}
        </p>
      )}

      <div className={styles.orgRow}>
        <input
          className={styles.input}
          value={name}
          onChange={(event) => setName(event.target.value)}
          placeholder="Acme Corp"
          aria-label="Organizasyon adı"
          required
        />
        <button className={styles.submit} type="submit" disabled={submitting}>
          {submitting ? 'Açılıyor…' : 'Organizasyon aç'}
        </button>
      </div>
    </form>
  );
}
