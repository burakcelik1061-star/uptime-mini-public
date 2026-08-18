'use client';

import Link from 'next/link';
import { useState, type FormEvent } from 'react';

import { ApiError } from '@/lib/api';
import type { Monitor, MonitorMethod } from '@/lib/api-types';
import styles from './MonitorForm.module.css';

/**
 * Monitor ekleme/düzenleme formu.
 *
 * TEK BİLEŞEN İKİ İŞ: `initial` verilirse düzenleme, verilmezse ekleme.
 * İkinci bir "edit" formu yazmak, alanların ve doğrulama mesajlarının iki
 * yerde ayrışması demekti (2.1'de eklenen `degradedResponseMs` alanı bunun
 * canlı örneği olurdu).
 *
 * DOĞRULAMA SUNUCUDA: burada `required` dışında kural tekrarlanmıyor.
 * Sınırları (30 sn, 1000 ms, 100-599) istemciye kopyalamak, iki yerde
 * ayrışabilen ikinci bir kural seti demek olurdu; sunucunun 422 cevabı zaten
 * ALAN BAZLI (`fields`) geliyor ve doğrudan ilgili girdinin altına
 * yazılabiliyor. Tarayıcının kendi `min`/`max` işaretleri yalnızca ipucu.
 */

const METHODS: MonitorMethod[] = ['GET', 'HEAD', 'POST'];

/** Formun ürettiği değerler; `undefined` = "gönderme" (sunucu varsayılanı
 *  ya da düzenlemede mevcut değer korunur). */
export interface MonitorFormValues {
  name: string;
  url: string;
  method: MonitorMethod;
  checkIntervalSeconds?: number;
  timeoutMs?: number;
  expectedStatusMin?: number;
  expectedStatusMax?: number;
  /** null = "yavaş" eşiğini KALDIR (düzenleme modunda anlamlı). */
  degradedResponseMs?: number | null;
}

interface Props {
  onSubmit: (values: MonitorFormValues) => Promise<void>;
  /** Verilirse form düzenleme modunda açılır. */
  initial?: Monitor;
  title?: string;
  submitLabel?: string;
  onCancel?: () => void;
}

/** Boş bırakılan sayısal alan gönderilmiyor. */
function optionalNumber(raw: string): number | undefined {
  const trimmed = raw.trim();
  if (trimmed === '') return undefined;

  const parsed = Number(trimmed);

  return Number.isFinite(parsed) ? parsed : undefined;
}

/** Sayısal alanın form değeri (yoksa boş dize). */
function initialText(value: number | null | undefined): string {
  return value === null || value === undefined ? '' : String(value);
}

export function MonitorForm({
  onSubmit,
  initial,
  title = 'Yeni monitor',
  submitLabel = 'Monitor ekle',
  onCancel,
}: Props) {
  const editing = initial !== undefined;

  const [name, setName] = useState(initial?.name ?? '');
  const [url, setUrl] = useState(initial?.url ?? '');
  const [method, setMethod] = useState<MonitorMethod>(initial?.method ?? 'GET');
  const [interval, setInterval] = useState(
    initialText(initial?.checkIntervalSeconds),
  );
  const [timeout, setTimeout] = useState(initialText(initial?.timeoutMs));
  const [statusMin, setStatusMin] = useState(
    initialText(initial?.expectedStatusMin),
  );
  const [statusMax, setStatusMax] = useState(
    initialText(initial?.expectedStatusMax),
  );
  const [degraded, setDegraded] = useState(
    initialText(initial?.degradedResponseMs),
  );

  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState<string | null>(null);
  // planBlocked, reddin sebebinin PLAN KOTASI olduğunu söylüyor (8.5).
  // Mesajı sunucu yazıyor; arayüzün eklediği tek şey ÇIKIŞ YOLU.
  const [planBlocked, setPlanBlocked] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    setFieldErrors({});
    setPlanBlocked(false);

    const degradedValue = optionalNumber(degraded);

    try {
      await onSubmit({
        name,
        url,
        method,
        checkIntervalSeconds: optionalNumber(interval),
        timeoutMs: optionalNumber(timeout),
        expectedStatusMin: optionalNumber(statusMin),
        expectedStatusMax: optionalNumber(statusMax),
        // Düzenlemede boş bırakmak "eşiği kaldır" demek (null); eklemede
        // "gönderme" demek (undefined). Sunucu bu ikisini ayırt ediyor.
        degradedResponseMs:
          degradedValue === undefined && editing ? null : degradedValue,
      });

      if (!editing) {
        setName('');
        setUrl('');
        setInterval('');
        setTimeout('');
        setStatusMin('');
        setStatusMax('');
        setDegraded('');
      }
    } catch (cause) {
      if (cause instanceof ApiError) {
        setFieldErrors(cause.fieldMessages());
        setError(cause.message);
        setPlanBlocked(cause.code === 'plan_limit_reached');
      } else {
        setError('Kaydedilemedi');
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form className={styles.form} onSubmit={handleSubmit} noValidate>
      <h2 className={styles.title}>{title}</h2>

      {error && (
        <p className={styles.error} role="alert">
          {error}
          {/* ARAYÜZ KURALI TEKRARLAMIYOR (4.4'ün kararı): sınırın ne olduğunu
              sunucunun mesajı söylüyor, buradaki tek ekleme ÇIKMAZ YOLU
              açmak — kullanıcıyı "olmadı" ile baş başa bırakmamak. */}
          {planBlocked && (
            <>
              {' '}
              <Link className={styles.planLink} href="/plan">
                Planı görüntüle
              </Link>
            </>
          )}
        </p>
      )}

      <div className={styles.row}>
        <label className={styles.field}>
          <span className={styles.label}>Ad</span>
          <input
            className={styles.input}
            value={name}
            onChange={(event) => setName(event.target.value)}
            placeholder="Ödeme servisi"
            required
            aria-invalid={fieldErrors.name ? true : undefined}
          />
          {fieldErrors.name && (
            <span className={styles.fieldError}>{fieldErrors.name}</span>
          )}
        </label>

        <label className={`${styles.field} ${styles.grow}`}>
          <span className={styles.label}>URL</span>
          <input
            className={styles.input}
            value={url}
            onChange={(event) => setUrl(event.target.value)}
            placeholder="https://example.com/health"
            required
            aria-invalid={fieldErrors.url ? true : undefined}
          />
          {fieldErrors.url && (
            <span className={styles.fieldError}>{fieldErrors.url}</span>
          )}
        </label>

        <label className={styles.field}>
          <span className={styles.label}>Metot</span>
          <select
            className={styles.input}
            value={method}
            onChange={(event) => setMethod(event.target.value as MonitorMethod)}
          >
            {METHODS.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </select>
        </label>
      </div>

      {/* Gelişmiş ayarlar katlanmış: yeni bir monitor için ad ve URL yeterli,
          gerisinin varsayılanı var. Düzenlemede açık başlıyor — kullanıcı
          zaten var olan değerleri değiştirmeye geldi. */}
      <details className={styles.advanced} open={editing}>
        <summary className={styles.summary}>Gelişmiş ayarlar</summary>

        <div className={styles.row}>
          <label className={styles.field}>
            <span className={styles.label}>Kontrol aralığı (sn)</span>
            <input
              className={styles.input}
              type="number"
              inputMode="numeric"
              min={30}
              max={86400}
              value={interval}
              onChange={(event) => setInterval(event.target.value)}
              placeholder="60"
              aria-invalid={fieldErrors.checkIntervalSeconds ? true : undefined}
            />
            {fieldErrors.checkIntervalSeconds && (
              <span className={styles.fieldError}>
                {fieldErrors.checkIntervalSeconds}
              </span>
            )}
          </label>

          <label className={styles.field}>
            <span className={styles.label}>Zaman aşımı (ms)</span>
            <input
              className={styles.input}
              type="number"
              inputMode="numeric"
              min={1000}
              max={60000}
              value={timeout}
              onChange={(event) => setTimeout(event.target.value)}
              placeholder="10000"
              aria-invalid={fieldErrors.timeoutMs ? true : undefined}
            />
            {fieldErrors.timeoutMs && (
              <span className={styles.fieldError}>{fieldErrors.timeoutMs}</span>
            )}
          </label>

          <label className={styles.field}>
            <span className={styles.label}>“Yavaş” eşiği (ms)</span>
            <input
              className={styles.input}
              type="number"
              inputMode="numeric"
              min={1}
              max={60000}
              value={degraded}
              onChange={(event) => setDegraded(event.target.value)}
              placeholder="kapalı"
              aria-invalid={fieldErrors.degradedResponseMs ? true : undefined}
            />
            {fieldErrors.degradedResponseMs ? (
              <span className={styles.fieldError}>
                {fieldErrors.degradedResponseMs}
              </span>
            ) : (
              <span className={styles.hint}>
                Boş bırakılırsa yavaşlık sinyali kapalı.
              </span>
            )}
          </label>

          <label className={styles.field}>
            <span className={styles.label}>Beklenen status (alt)</span>
            <input
              className={styles.input}
              type="number"
              inputMode="numeric"
              min={100}
              max={599}
              value={statusMin}
              onChange={(event) => setStatusMin(event.target.value)}
              placeholder="200"
              aria-invalid={fieldErrors.expectedStatusMin ? true : undefined}
            />
            {fieldErrors.expectedStatusMin && (
              <span className={styles.fieldError}>
                {fieldErrors.expectedStatusMin}
              </span>
            )}
          </label>

          <label className={styles.field}>
            <span className={styles.label}>Beklenen status (üst)</span>
            <input
              className={styles.input}
              type="number"
              inputMode="numeric"
              min={100}
              max={599}
              value={statusMax}
              onChange={(event) => setStatusMax(event.target.value)}
              placeholder="299"
              aria-invalid={fieldErrors.expectedStatusMax ? true : undefined}
            />
            {fieldErrors.expectedStatusMax && (
              <span className={styles.fieldError}>
                {fieldErrors.expectedStatusMax}
              </span>
            )}
          </label>
        </div>
      </details>

      <div className={styles.actions}>
        {onCancel && (
          <button
            className={styles.secondary}
            type="button"
            onClick={onCancel}
            disabled={submitting}
          >
            Vazgeç
          </button>
        )}
        <button className={styles.submit} type="submit" disabled={submitting}>
          {submitting ? 'Kaydediliyor…' : submitLabel}
        </button>
      </div>
    </form>
  );
}
