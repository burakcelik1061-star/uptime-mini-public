'use client';

import { useRouter } from 'next/navigation';
import { useEffect, useState, type FormEvent } from 'react';

import { AppHeader } from '@/components/AppHeader';
import { ApiError } from '@/lib/api';
import type {
  Invitation,
  Organization,
  OrganizationMember,
  OrgRole,
} from '@/lib/api-types';
import { useSession } from '@/lib/session';
import styles from './page.module.css';

/**
 * EKİP EKRANI (8.3): üyeler, bekleyen davetler ve davet formu.
 *
 * 1.3/1.4 üyeliği ve rolleri kurmuştu ama arayüzü yoktu; ikinci bir kişi
 * organizasyona ancak elle SQL ile giriyordu — yani "multi-tenant" pratikte
 * tek kullanıcılıydı.
 *
 * ARAYÜZ YETKİ KURALINI TEKRARLAMIYOR, yalnızca çıkmaz yola sokmuyor (4.4'ün
 * kararı): `member` rolündeki kullanıcıya davet formu ve iptal düğmesi
 * GÖSTERİLMİYOR, ama kuralın kendisi sunucuda (403). Aynı şekilde rol
 * seçicisinde kullanıcının KENDİ rolünün üstü listelenmiyor — yükseltmeyi
 * engelleyen şey bu liste değil, sunucudaki kapı.
 */

const ROLE_LABELS: Record<OrgRole, string> = {
  owner: 'Sahip',
  admin: 'Yönetici',
  member: 'Üye',
};

/** Rol sıralaması — 1.4'ün `roleRank`ının arayüzdeki karşılığı. */
const ROLE_RANK: Record<OrgRole, number> = { member: 1, admin: 2, owner: 3 };

const ALL_ROLES: OrgRole[] = ['member', 'admin', 'owner'];

function formatDate(value: string): string {
  return new Date(value).toLocaleDateString('tr-TR');
}

export default function TeamPage() {
  const router = useRouter();
  const { status, authFetch } = useSession();

  const [org, setOrg] = useState<Organization | null>(null);
  const [members, setMembers] = useState<OrganizationMember[]>([]);
  const [invitations, setInvitations] = useState<Invitation[]>([]);

  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [reloadToken, setReloadToken] = useState(0);

  const [email, setEmail] = useState('');
  const [role, setRole] = useState<OrgRole>('member');
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);

  useEffect(() => {
    if (status === 'anonymous') router.replace('/login');
  }, [status, router]);

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
          setMembers([]);
          setInvitations([]);
          return;
        }

        const [memberData, inviteData] = await Promise.all([
          authFetch<{ members: OrganizationMember[] }>(
            `/v1/orgs/${first.id}/members`,
          ),
          authFetch<{ invitations: Invitation[] }>(
            `/v1/orgs/${first.id}/invitations`,
          ),
        ]);
        if (cancelled) return;

        setMembers(memberData.members);
        setInvitations(inviteData.invitations);
      } catch (cause) {
        if (cancelled) return;
        setError(cause instanceof ApiError ? cause.message : 'Ekip okunamadı');
      } finally {
        if (!cancelled) setLoading(false);
      }
    }

    void load();

    return () => {
      cancelled = true;
    };
  }, [status, authFetch, reloadToken]);

  const canManage = org !== null && ROLE_RANK[org.role] >= ROLE_RANK.admin;
  // Kendi rolünün ÜSTÜNÜ seçenek olarak göstermiyoruz. Bu bir güvenlik
  // önlemi DEĞİL (kural sunucuda) — kullanıcıyı reddedilecek bir seçeneğe
  // sürüklememek için.
  const assignableRoles = org
    ? ALL_ROLES.filter((value) => ROLE_RANK[value] <= ROLE_RANK[org.role])
    : [];

  async function handleInvite(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!org) return;

    setSubmitting(true);
    setError(null);
    setNotice(null);
    setFieldErrors({});

    try {
      await authFetch(`/v1/orgs/${org.id}/invitations`, {
        method: 'POST',
        body: { email, role },
      });

      // MESAJ "GÖNDERİLDİ" DİYOR, "kaydedildi" demiyor: sunucu zaten
      // bekleyen bir davet varsa yenisini yazmıyor ve bunu SÖYLEMİYOR
      // (enumeration kararı). Arayüz o ayrımı uydurmuyor.
      setNotice(`${email} adresine davet gönderildi.`);
      setEmail('');
      setReloadToken((token) => token + 1);
    } catch (cause) {
      if (cause instanceof ApiError) {
        setFieldErrors(cause.fieldMessages());
        setError(cause.message);
      } else {
        setError('Davet gönderilemedi');
      }
    } finally {
      setSubmitting(false);
    }
  }

  async function handleRevoke(invitation: Invitation) {
    if (!org) return;

    setBusy(invitation.id);
    setError(null);
    setNotice(null);

    try {
      await authFetch(`/v1/orgs/${org.id}/invitations/${invitation.id}`, {
        method: 'DELETE',
      });
      setNotice(`${invitation.email} daveti iptal edildi.`);
      setReloadToken((token) => token + 1);
    } catch (cause) {
      setError(
        cause instanceof ApiError ? cause.message : 'Davet iptal edilemedi',
      );
    } finally {
      setBusy(null);
    }
  }

  if (status !== 'authenticated') {
    return <main className={styles.page}>Yükleniyor…</main>;
  }

  return (
    <>
      <AppHeader org={org} />

      <main className={styles.page}>
        <h1 className={styles.title}>Ekip</h1>

        {error && (
          <p className={styles.error} role="alert">
            {error}
          </p>
        )}
        {notice && (
          <p className={styles.notice} role="status">
            {notice}
          </p>
        )}

        {!loading && !org && (
          <p className={styles.empty}>
            Önce bir organizasyon açın; ekip davetleri organizasyona ait.
          </p>
        )}

        {org && (
          <>
            {canManage && (
              <form
                className={styles.inviteForm}
                onSubmit={handleInvite}
                noValidate
              >
                <h2 className={styles.sectionTitle}>Ekip arkadaşı davet et</h2>

                <div className={styles.inviteRow}>
                  <label className={styles.field}>
                    <span className={styles.label}>E-posta</span>
                    <input
                      className={styles.input}
                      type="email"
                      value={email}
                      onChange={(event) => setEmail(event.target.value)}
                      placeholder="ekip@sirket.com"
                      required
                      aria-invalid={fieldErrors.email ? true : undefined}
                    />
                    {fieldErrors.email && (
                      <span className={styles.fieldError}>
                        {fieldErrors.email}
                      </span>
                    )}
                  </label>

                  <label className={styles.field}>
                    <span className={styles.label}>Rol</span>
                    <select
                      className={styles.input}
                      value={role}
                      onChange={(event) =>
                        setRole(event.target.value as OrgRole)
                      }
                    >
                      {assignableRoles.map((value) => (
                        <option key={value} value={value}>
                          {ROLE_LABELS[value]}
                        </option>
                      ))}
                    </select>
                    {fieldErrors.role && (
                      <span className={styles.fieldError}>
                        {fieldErrors.role}
                      </span>
                    )}
                  </label>

                  <button
                    className={styles.submit}
                    type="submit"
                    disabled={submitting}
                  >
                    {submitting ? 'Gönderiliyor…' : 'Davet gönder'}
                  </button>
                </div>
              </form>
            )}

            <section>
              <h2 className={styles.sectionTitle}>Üyeler</h2>
              <ul className={styles.list}>
                {members.map((member) => (
                  <li key={member.userId} className={styles.row}>
                    <div className={styles.rowMain}>
                      <span className={styles.name}>{member.name}</span>
                      <span className={styles.address}>{member.email}</span>
                    </div>
                    {/* Rol renkle değil METİNLE anlatılıyor (0.2'nin şartı). */}
                    <span className={styles.role}>
                      {ROLE_LABELS[member.role]}
                    </span>
                    <span className={styles.meta}>
                      {formatDate(member.joinedAt)}
                    </span>
                  </li>
                ))}
              </ul>
            </section>

            <section>
              <h2 className={styles.sectionTitle}>Bekleyen davetler</h2>

              {invitations.length === 0 ? (
                <p className={styles.empty}>Bekleyen davet yok.</p>
              ) : (
                <ul className={styles.list}>
                  {invitations.map((invitation) => {
                    const expired = new Date(invitation.expiresAt) < new Date();

                    return (
                      <li key={invitation.id} className={styles.row}>
                        <div className={styles.rowMain}>
                          <span className={styles.address}>
                            {invitation.email}
                          </span>
                          {invitation.invitedByEmail && (
                            <span className={styles.meta}>
                              Davet eden: {invitation.invitedByEmail}
                            </span>
                          )}
                        </div>
                        <span className={styles.role}>
                          {ROLE_LABELS[invitation.role]}
                        </span>
                        {/* "Süresi doldu" durumu renkle değil METİNLE. */}
                        <span className={styles.meta}>
                          {expired
                            ? 'Süresi doldu'
                            : `${formatDate(invitation.expiresAt)} tarihine kadar`}
                        </span>
                        {canManage && (
                          <button
                            className={styles.revoke}
                            type="button"
                            onClick={() => void handleRevoke(invitation)}
                            disabled={busy === invitation.id}
                          >
                            {busy === invitation.id
                              ? 'İptal ediliyor…'
                              : 'İptal et'}
                          </button>
                        )}
                      </li>
                    );
                  })}
                </ul>
              )}
            </section>
          </>
        )}
      </main>
    </>
  );
}
