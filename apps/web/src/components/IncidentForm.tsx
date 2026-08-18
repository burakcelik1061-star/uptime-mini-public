'use client';

import { useState, type FormEvent } from 'react';

import { ApiError } from '@/lib/api';
import type {
  CreateIncidentInput,
  IncidentImpact,
  Monitor,
} from '@/lib/api-types';
import styles from './FormControls.module.css';

/**
 * Manuel incident oluşturma (4.4).
 *
 * Otomatik incident'tan iki farkı burada da görünüyor (0.1 §5): BİRDEN FAZLA
 * servis seçilebiliyor ve ETKİYİ kullanıcı seçiyor — sistem bir servisin iş
 * açısından önemini bilmiyor, o yüzden otomatik kayıtlar hep `major`.
 *
 * DOĞRULAMA SUNUCUDA (2.1'in kuralı): burada `required` dışında kural
 * tekrarlanmıyor; 422 cevabının `fields` dizisi doğrudan ilgili girdinin
 * altına yazılıyor.
 *
 * BAŞLANGIÇ ZAMANI ALANI YOK: sunucu `now()` yazıyor. Geçmişe tarihlenen bir
 * incident, kesinti geçmişini elle yazmanın kapısı olurdu (4.3'ün "geçmişe
 * dönük bakım penceresi yok" kararıyla aynı gerekçe).
 */

const IMPACTS: { value: IncidentImpact; label: string }[] = [
  { value: 'none', label: 'Etkisiz — bilgilendirme' },
  { value: 'minor', label: 'Küçük — sınırlı etki' },
  { value: 'major', label: 'Büyük — servis kullanılamıyor' },
  { value: 'critical', label: 'Kritik — yaygın kesinti' },
];

interface Props {
  monitors: Monitor[];
  onSubmit: (values: CreateIncidentInput) => Promise<void>;
  onCancel?: () => void;
}

export function IncidentForm({ monitors, onSubmit, onCancel }: Props) {
  const [title, setTitle] = useState('');
  const [impact, setImpact] = useState<IncidentImpact>('major');
  const [body, setBody] = useState('');
  const [selected, setSelected] = useState<string[]>([]);

  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  function toggleMonitor(id: string) {
    setSelected((current) =>
      current.includes(id)
        ? current.filter((value) => value !== id)
        : [...current, id],
    );
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    setFieldErrors({});

    try {
      await onSubmit({ title, impact, monitorIds: selected, body });

      setTitle('');
      setBody('');
      setSelected([]);
      setImpact('major');
    } catch (cause) {
      if (cause instanceof ApiError) {
        setFieldErrors(cause.fieldMessages());
        setError(cause.message);
      } else {
        setError('Incident açılamadı');
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form className={styles.form} onSubmit={handleSubmit} noValidate>
      <h2 className={styles.title}>Manuel incident aç</h2>

      {error && (
        <p className={styles.error} role="alert">
          {error}
        </p>
      )}

      <div className={styles.row}>
        <label className={`${styles.field} ${styles.grow}`}>
          <span className={styles.label}>Başlık</span>
          <input
            className={styles.input}
            value={title}
            onChange={(event) => setTitle(event.target.value)}
            placeholder="Veritabanı bakımı kesintiye yol açtı"
            required
            aria-invalid={fieldErrors.title ? true : undefined}
          />
          {fieldErrors.title && (
            <span className={styles.fieldError}>{fieldErrors.title}</span>
          )}
        </label>

        <label className={styles.field}>
          <span className={styles.label}>Etki</span>
          <select
            className={styles.input}
            value={impact}
            onChange={(event) =>
              setImpact(event.target.value as IncidentImpact)
            }
            aria-invalid={fieldErrors.impact ? true : undefined}
          >
            {IMPACTS.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
          {fieldErrors.impact && (
            <span className={styles.fieldError}>{fieldErrors.impact}</span>
          )}
        </label>
      </div>

      <fieldset className={styles.services}>
        <legend className={styles.servicesLegend}>Etkilenen servisler</legend>

        {monitors.length === 0 ? (
          <span className={styles.hint}>
            Önce bir monitor eklemelisiniz: incident en az bir servisi kapsamak
            zorunda.
          </span>
        ) : (
          monitors.map((monitor) => (
            <label key={monitor.id} className={styles.serviceOption}>
              <input
                type="checkbox"
                checked={selected.includes(monitor.id)}
                onChange={() => toggleMonitor(monitor.id)}
              />
              {monitor.name}
            </label>
          ))
        )}

        {fieldErrors.monitorIds && (
          <span className={styles.fieldError}>{fieldErrors.monitorIds}</span>
        )}
      </fieldset>

      <label className={styles.field}>
        <span className={styles.label}>İlk güncelleme</span>
        <textarea
          className={`${styles.input} ${styles.textarea}`}
          value={body}
          onChange={(event) => setBody(event.target.value)}
          placeholder="Sorunu araştırıyoruz."
          required
          aria-invalid={fieldErrors.body ? true : undefined}
        />
        {fieldErrors.body ? (
          <span className={styles.fieldError}>{fieldErrors.body}</span>
        ) : (
          <span className={styles.hint}>
            Timeline&apos;ın ilk satırı olur; incident “araştırılıyor” durumunda
            açılır.
          </span>
        )}
      </label>

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
          {submitting ? 'Açılıyor…' : 'Incident aç'}
        </button>
      </div>
    </form>
  );
}
