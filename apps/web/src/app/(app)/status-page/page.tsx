'use client';

import { useRouter } from 'next/navigation';
import { useCallback, useEffect, useMemo, useState } from 'react';

import { AppHeader } from '@/components/AppHeader';
import { PublicStatusView } from '@/components/PublicStatusView';
import { ApiError } from '@/lib/api';
import type {
  Monitor,
  Organization,
  PublicStatusPage,
  StatusPageResponse,
  UpdateStatusPageInput,
} from '@/lib/api-types';
import { previewPage, type Appearance } from '@/lib/status-page-preview';
import { useSession } from '@/lib/session';
import styles from './page.module.css';

/**
 * Status page yönetimi (5.1'in seçimi + 5.3'ün özelleştirmesi).
 *
 * Üç karar veriliyor ve üçü de "dışarıya ne görünüyor" sorusunun cevabı:
 *   1. Sayfa YAYIMDA MI (yayımlanmamış sayfa ziyaretçiye 404 dönüyor).
 *   2. Hangi servisler yayımlanıyor ve hangi grupta.
 *   3. Sayfa nasıl görünüyor (başlık, açıklama, logo, tema rengi).
 *
 * SEÇİM VE GRUPLAMA TÜMDEN GÖNDERİLİYOR (PUT …/services): "ekle/çıkar" uçları,
 * ekrandaki seçim ile sunucudaki seçimin sessizce ayrışmasına açıktı ve o
 * ayrışmanın adı "yayımlanmaması gereken bir servis yayımda kaldı".
 *
 * CANLI ÖNİZLEME, PUBLIC SAYFANIN KENDİ BİLEŞENİ (`PublicStatusView`).
 * "Benzer" bir düzen yazsaydık, önizlemede göründüğü gibi yayımlanmayan bir
 * sayfa kaçınılmazdı — ve fark ancak yayımladıktan sonra görülürdü.
 */
/**
 * Kiracının alt alan adı adresi (5.4).
 *
 * Sunucudan gelmiyor: ana alan adı yalnızca Next middleware'inde okunuyor
 * (`STATUS_PAGE_ROOT_DOMAIN`) ve api onu hiç bilmiyor. Panonun açıldığı adres
 * zaten doğru cevabı taşıyor — pano ana alan adında açılır.
 */
function subdomainAddress(slug: string): string {
  if (typeof window === 'undefined') return '';

  const { protocol, hostname, port } = window.location;

  return `${protocol}//${slug}.${hostname}${port ? `:${port}` : ''}`;
}

export default function StatusPageSettingsScreen() {
  const router = useRouter();
  const { status, authFetch } = useSession();

  const [org, setOrg] = useState<Organization | null>(null);
  const [data, setData] = useState<StatusPageResponse | null>(null);
  const [monitors, setMonitors] = useState<Monitor[]>([]);

  // Kaydedilmemiş taslak: seçim, gruplama ve görünüm.
  const [selected, setSelected] = useState<Map<string, string | null>>(
    new Map(),
  );
  const [groups, setGroups] = useState<string[]>([]);
  const [appearance, setAppearance] = useState<Appearance>({
    title: '',
    description: '',
    logoUrl: '',
    themeColor: '',
  });
  const [newGroup, setNewGroup] = useState('');

  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);

  useEffect(() => {
    if (status === 'anonymous') router.replace('/login');
  }, [status, router]);

  const applyResponse = useCallback((response: StatusPageResponse) => {
    setData(response);
    setGroups(response.groups);
    setSelected(
      new Map(response.items.map((item) => [item.monitorId, item.group])),
    );
    setAppearance({
      title: response.statusPage.title ?? '',
      description: response.statusPage.description ?? '',
      logoUrl: response.statusPage.logoUrl ?? '',
      themeColor: response.statusPage.themeColor ?? '',
    });
  }, []);

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

        if (!first) return;

        const [page, monitorData] = await Promise.all([
          authFetch<StatusPageResponse>(`/v1/orgs/${first.id}/status-page`),
          authFetch<{ monitors: Monitor[] }>(`/v1/orgs/${first.id}/monitors`),
        ]);

        if (cancelled) return;

        applyResponse(page);
        setMonitors(monitorData.monitors);
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
  }, [status, authFetch, applyResponse]);

  const toggle = useCallback((monitorID: string) => {
    setSelected((current) => {
      const next = new Map(current);
      if (next.has(monitorID)) next.delete(monitorID);
      else next.set(monitorID, null);

      return next;
    });
  }, []);

  const assignGroup = useCallback((monitorID: string, group: string | null) => {
    setSelected((current) => new Map(current).set(monitorID, group));
  }, []);

  const addGroup = useCallback(() => {
    const name = newGroup.trim();
    if (name === '') return;

    setGroups((current) =>
      current.includes(name) ? current : [...current, name],
    );
    setNewGroup('');
  }, [newGroup]);

  const removeGroup = useCallback((name: string) => {
    setGroups((current) => current.filter((group) => group !== name));
    // Gruptaki servisler YAYIMDA KALIYOR, yalnızca gruplaması düşüyor
    // (sunucudaki ON DELETE SET NULL ile aynı davranış).
    setSelected((current) => {
      const next = new Map(current);
      for (const [monitorID, group] of next) {
        if (group === name) next.set(monitorID, null);
      }

      return next;
    });
  }, []);

  const moveGroup = useCallback((index: number, delta: number) => {
    setGroups((current) => {
      const target = index + delta;
      if (target < 0 || target >= current.length) return current;

      const next = [...current];
      [next[index], next[target]] = [next[target], next[index]];

      return next;
    });
  }, []);

  const saveServices = useCallback(async () => {
    if (!org) return;

    setBusy(true);
    setError(null);
    setSaved(null);

    try {
      const response = await authFetch<StatusPageResponse>(
        `/v1/orgs/${org.id}/status-page/services`,
        {
          method: 'PUT',
          body: {
            groups,
            items: [...selected].map(([monitorId, group]) => ({
              monitorId,
              group,
            })),
          },
        },
      );

      applyResponse(response);
      setSaved('Servisler kaydedildi.');
    } catch (cause) {
      setError(cause instanceof ApiError ? cause.message : 'Kaydedilemedi');
    } finally {
      setBusy(false);
    }
  }, [org, authFetch, groups, selected, applyResponse]);

  const patchSettings = useCallback(
    async (input: UpdateStatusPageInput, message: string) => {
      if (!org) return;

      setBusy(true);
      setError(null);
      setSaved(null);

      try {
        const response = await authFetch<StatusPageResponse>(
          `/v1/orgs/${org.id}/status-page`,
          { method: 'PATCH', body: input },
        );

        applyResponse(response);
        setSaved(message);
      } catch (cause) {
        setError(cause instanceof ApiError ? cause.message : 'Kaydedilemedi');
      } finally {
        setBusy(false);
      }
    },
    [org, authFetch, applyResponse],
  );

  const saveAppearance = useCallback(
    () =>
      patchSettings(
        {
          // Boş alan `null` gönderiliyor: "temizle" ile "dokunma" ayrı
          // (sözleşme üç durumlu).
          title: appearance.title.trim() || null,
          description: appearance.description.trim() || null,
          logoUrl: appearance.logoUrl.trim() || null,
          themeColor: appearance.themeColor.trim() || null,
        },
        'Görünüm kaydedildi.',
      ),
    [patchSettings, appearance],
  );

  // ÖNİZLEME: public sayfanın gerçek bileşeni, taslak ayarlarla beslenmiş
  // hâli. Kesinti/bakım/geçmiş verisi taşımıyor — önizlemenin sorusu
  // "nasıl görünüyor", "şu an ne oluyor" değil.
  const preview: PublicStatusPage | null = useMemo(
    () =>
      data === null
        ? null
        : previewPage({
            slug: data.statusPage.slug,
            orgName: org?.name ?? '',
            appearance,
            groups,
            services: monitors
              .filter((monitor) => selected.has(monitor.id))
              .map((monitor) => ({
                id: monitor.id,
                name: monitor.name,
                status: monitor.status,
                group: selected.get(monitor.id) ?? null,
              })),
          }),
    [data, org, appearance, groups, monitors, selected],
  );

  if (status !== 'authenticated') {
    return <main className={styles.page}>Yükleniyor…</main>;
  }

  const canManage = org !== null && org.role !== 'member';
  const settings = data?.statusPage ?? null;

  return (
    <div className={styles.shell}>
      <AppHeader org={org} />

      <main className={styles.page}>
        <h1 className={styles.title}>Status page</h1>

        <p className={styles.lead}>
          Yalnızca <strong>seçtiğiniz servisler</strong> herkese açık sayfada
          görünür. Seçilmeyen bir servisin adı, onu etkileyen kesintilerin
          başlığı dahil, hiçbir yerde geçmez.
        </p>

        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}
        {saved && (
          <p className={styles.saved} role="status">
            {saved}
          </p>
        )}

        {loading || !settings ? (
          <p className={styles.muted}>Yükleniyor…</p>
        ) : (
          <div className={styles.columns}>
            <div className={styles.editor}>
              <section className={styles.card}>
                <div className={styles.cardHead}>
                  <h2 className={styles.cardTitle}>Yayın durumu</h2>
                  {/* Durum yalnızca renkle değil METİNLE anlatılıyor. */}
                  <span
                    className={`${styles.state} ${
                      settings.isPublished ? styles.live : styles.draft
                    }`}
                  >
                    {settings.isPublished ? 'Yayında' : 'Yayımlanmadı'}
                  </span>
                </div>

                <p className={styles.address}>
                  Adres:{' '}
                  <code className="tabularNumbers">{settings.publicPath}</code>
                </p>

                {/* Alt alan adı (5.4). Adres TARAYICIDAN türetiliyor: ana
                    alan adını sunucu bilmiyor (o değer yalnızca Next
                    middleware'inde okunuyor) ve panonun açıldığı adres zaten
                    doğru cevabı taşıyor. */}
                <p className={styles.address}>
                  Alt alan adı:{' '}
                  <code className="tabularNumbers">
                    {subdomainAddress(settings.slug)}
                  </code>
                </p>

                {settings.isPublished ? (
                  <a
                    className={styles.link}
                    href={settings.publicPath}
                    target="_blank"
                    rel="noreferrer"
                  >
                    Sayfayı aç
                  </a>
                ) : (
                  <p className={styles.muted}>
                    Yayımlanmamış bir sayfa ziyaretçiye “böyle bir sayfa yok”
                    cevabı verir.
                  </p>
                )}

                {canManage && (
                  <button
                    className={styles.primary}
                    type="button"
                    onClick={() =>
                      void patchSettings(
                        { isPublished: !settings.isPublished },
                        settings.isPublished
                          ? 'Sayfa yayımdan kaldırıldı.'
                          : 'Sayfa yayımlandı.',
                      )
                    }
                    disabled={busy}
                  >
                    {settings.isPublished ? 'Yayımdan kaldır' : 'Yayımla'}
                  </button>
                )}
              </section>

              <section className={styles.card}>
                <h2 className={styles.cardTitle}>Görünüm</h2>

                <label className={styles.field}>
                  <span className={styles.label}>Başlık</span>
                  <input
                    className={styles.input}
                    value={appearance.title}
                    onChange={(event) =>
                      setAppearance((current) => ({
                        ...current,
                        title: event.target.value,
                      }))
                    }
                    placeholder={org?.name ?? 'Organizasyon adı'}
                    disabled={!canManage || busy}
                  />
                  <span className={styles.hint}>
                    Boş bırakılırsa organizasyonun adı kullanılır.
                  </span>
                </label>

                <label className={styles.field}>
                  <span className={styles.label}>Açıklama</span>
                  <textarea
                    className={styles.textarea}
                    value={appearance.description}
                    onChange={(event) =>
                      setAppearance((current) => ({
                        ...current,
                        description: event.target.value,
                      }))
                    }
                    rows={2}
                    disabled={!canManage || busy}
                  />
                </label>

                <label className={styles.field}>
                  <span className={styles.label}>Logo adresi</span>
                  <input
                    className={styles.input}
                    value={appearance.logoUrl}
                    onChange={(event) =>
                      setAppearance((current) => ({
                        ...current,
                        logoUrl: event.target.value,
                      }))
                    }
                    placeholder="https://…"
                    disabled={!canManage || busy}
                  />
                  <span className={styles.hint}>
                    Yalnızca https adresleri kabul edilir.
                  </span>
                </label>

                <div className={styles.field}>
                  <span className={styles.label}>Tema rengi</span>
                  <div className={styles.colorRow}>
                    <input
                      className={styles.color}
                      type="color"
                      value={appearance.themeColor || '#5b9dff'}
                      onChange={(event) =>
                        setAppearance((current) => ({
                          ...current,
                          themeColor: event.target.value,
                        }))
                      }
                      disabled={!canManage || busy}
                      aria-label="Tema rengi"
                    />
                    <code className={`${styles.colorValue} tabularNumbers`}>
                      {appearance.themeColor || 'varsayılan'}
                    </code>
                    {appearance.themeColor && canManage && (
                      <button
                        className={styles.ghost}
                        type="button"
                        onClick={() =>
                          setAppearance((current) => ({
                            ...current,
                            themeColor: '',
                          }))
                        }
                        disabled={busy}
                      >
                        Varsayılana dön
                      </button>
                    )}
                  </div>
                  <span className={styles.hint}>
                    Renk yalnızca kimlik için kullanılır: durum bilgisi her
                    zaman sabit renk + şekil + metinle anlatılır, yani seçiminiz
                    sayfayı okunmaz yapamaz.
                  </span>
                </div>

                {canManage && (
                  <button
                    className={styles.primary}
                    type="button"
                    onClick={() => void saveAppearance()}
                    disabled={busy}
                  >
                    Görünümü kaydet
                  </button>
                )}
              </section>

              <section className={styles.card}>
                <h2 className={styles.cardTitle}>Gruplar</h2>

                <p className={styles.hint}>
                  Grup silmek servisi yayımdan kaldırmaz, yalnızca gruplamasını
                  düşürür.
                </p>

                {groups.length === 0 ? (
                  <p className={styles.muted}>Grup tanımlanmadı.</p>
                ) : (
                  <ol className={styles.groupList}>
                    {groups.map((group, index) => (
                      <li key={group} className={styles.groupItem}>
                        <span className={styles.name}>{group}</span>

                        {canManage && (
                          <span className={styles.groupActions}>
                            <button
                              className={styles.ghost}
                              type="button"
                              onClick={() => moveGroup(index, -1)}
                              disabled={busy || index === 0}
                              aria-label={`${group} grubunu yukarı taşı`}
                            >
                              ↑
                            </button>
                            <button
                              className={styles.ghost}
                              type="button"
                              onClick={() => moveGroup(index, 1)}
                              disabled={busy || index === groups.length - 1}
                              aria-label={`${group} grubunu aşağı taşı`}
                            >
                              ↓
                            </button>
                            <button
                              className={styles.ghost}
                              type="button"
                              onClick={() => removeGroup(group)}
                              disabled={busy}
                            >
                              Sil
                            </button>
                          </span>
                        )}
                      </li>
                    ))}
                  </ol>
                )}

                {canManage && (
                  <div className={styles.actions}>
                    <input
                      className={styles.input}
                      value={newGroup}
                      onChange={(event) => setNewGroup(event.target.value)}
                      placeholder="Yeni grup adı"
                      disabled={busy}
                    />
                    <button
                      className={styles.ghost}
                      type="button"
                      onClick={addGroup}
                      disabled={busy || newGroup.trim() === ''}
                    >
                      Grup ekle
                    </button>
                  </div>
                )}
              </section>

              <section className={styles.card}>
                <h2 className={styles.cardTitle}>Yayımlanan servisler</h2>

                {monitors.length === 0 ? (
                  <p className={styles.empty}>
                    Henüz monitor yok. Önce bir servis ekleyin.
                  </p>
                ) : (
                  <ul className={styles.list}>
                    {monitors.map((monitor) => (
                      <li key={monitor.id} className={styles.item}>
                        <label className={styles.choice}>
                          <input
                            type="checkbox"
                            checked={selected.has(monitor.id)}
                            onChange={() => toggle(monitor.id)}
                            disabled={!canManage || busy}
                          />
                          <span className={styles.name}>{monitor.name}</span>
                          <span className={styles.url}>{monitor.url}</span>
                        </label>

                        {selected.has(monitor.id) && groups.length > 0 && (
                          <select
                            className={styles.select}
                            value={selected.get(monitor.id) ?? ''}
                            onChange={(event) =>
                              assignGroup(
                                monitor.id,
                                event.target.value === ''
                                  ? null
                                  : event.target.value,
                              )
                            }
                            disabled={!canManage || busy}
                            aria-label={`${monitor.name} grubu`}
                          >
                            <option value="">Gruplanmamış</option>
                            {groups.map((group) => (
                              <option key={group} value={group}>
                                {group}
                              </option>
                            ))}
                          </select>
                        )}
                      </li>
                    ))}
                  </ul>
                )}

                {canManage ? (
                  <button
                    className={styles.primary}
                    type="button"
                    onClick={() => void saveServices()}
                    disabled={busy}
                  >
                    {busy ? 'Kaydediliyor…' : 'Servisleri ve grupları kaydet'}
                  </button>
                ) : (
                  <p className={styles.muted}>
                    Bu organizasyondaki rolünüz salt-okunur; status page
                    ayarları admin ve üzerindedir.
                  </p>
                )}
              </section>
            </div>

            <aside className={styles.previewColumn} aria-label="Canlı önizleme">
              <h2 className={styles.cardTitle}>Canlı önizleme</h2>
              <p className={styles.hint}>
                Kaydedilmemiş değişiklikler dahil. Kesinti ve uptime geçmişi
                önizlemede gösterilmez.
              </p>

              {preview && (
                <div className={styles.preview}>
                  <PublicStatusView page={preview} />
                </div>
              )}
            </aside>
          </div>
        )}
      </main>
    </div>
  );
}
