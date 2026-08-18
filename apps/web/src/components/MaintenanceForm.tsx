'use client';

import { useState, type FormEvent } from 'react';

import { ApiError } from '@/lib/api';
import type { CreateMaintenanceWindowInput, Monitor } from '@/lib/api-types';
import styles from './FormControls.module.css';

/**
 * Planlı bakım penceresi tanımlama (4.3'ün ucu, 4.4'ün ekranı).
 *
 * `datetime-local` YEREL saat veriyor (saat dilimi taşımıyor); gönderirken
 * `toISOString()` ile UTC'ye çevriliyor. Kullanıcı kendi saatiyle düşünüyor,
 * sunucu tek bir zaman ekseninde çalışıyor — çeviri tam bu sınırda yapılmalı.
 *
 * Sunucu GEÇMİŞE DÖNÜK pencereyi reddediyor (bitiş gelecekte olmak zorunda);
 * o kural burada TEKRARLANMIYOR, 422 alan hatası olarak gösteriliyor.
 */

interface Props {
  monitors: Monitor[];
  onSubmit: (values: CreateMaintenanceWindowInput) => Promise<void>;
}

/** `datetime-local` değerini sözleşmenin beklediği RFC 3339'a çevirir. */
function toISO(localValue: string): string {
  if (localValue === '') return '';

  const parsed = new Date(localValue);

  return Number.isNaN(parsed.getTime()) ? '' : parsed.toISOString();
}

export function MaintenanceForm({ monitors, onSubmit }: Props) {
  const [title, setTitle] = useState('');
  const [description, setDescription] = useState('');
  const [startsAt, setStartsAt] = useState('');
  const [endsAt, setEndsAt] = useState('');
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
      await onSubmit({
        title,
        description: description.trim() === '' ? undefined : description,
        startsAt: toISO(startsAt),
        endsAt: toISO(endsAt),
        monitorIds: selected,
      });

      setTitle('');
      setDescription('');
      setStartsAt('');
      setEndsAt('');
      setSelected([]);
    } catch (cause) {
      if (cause instanceof ApiError) {
        setFieldErrors(cause.fieldMessages());
        setError(cause.message);
      } else {
        setError('Bakım penceresi oluşturulamadı');
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form className={styles.form} onSubmit={handleSubmit} noValidate>
      <h2 className={styles.title}>Yeni bakım penceresi</h2>

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
            placeholder="Veritabanı sürüm yükseltmesi"
            required
            aria-invalid={fieldErrors.title ? true : undefined}
          />
          {fieldErrors.title && (
            <span className={styles.fieldError}>{fieldErrors.title}</span>
          )}
        </label>

        <label className={styles.field}>
          <span className={styles.label}>Başlangıç</span>
          <input
            className={styles.input}
            type="datetime-local"
            value={startsAt}
            onChange={(event) => setStartsAt(event.target.value)}
            required
            aria-invalid={fieldErrors.startsAt ? true : undefined}
          />
          {fieldErrors.startsAt && (
            <span className={styles.fieldError}>{fieldErrors.startsAt}</span>
          )}
        </label>

        <label className={styles.field}>
          <span className={styles.label}>Bitiş</span>
          <input
            className={styles.input}
            type="datetime-local"
            value={endsAt}
            onChange={(event) => setEndsAt(event.target.value)}
            required
            aria-invalid={fieldErrors.endsAt ? true : undefined}
          />
          {fieldErrors.endsAt ? (
            <span className={styles.fieldError}>{fieldErrors.endsAt}</span>
          ) : (
            <span className={styles.hint}>Bitiş gelecekte olmalı.</span>
          )}
        </label>
      </div>

      <fieldset className={styles.services}>
        <legend className={styles.servicesLegend}>Kapsanan servisler</legend>

        {monitors.length === 0 ? (
          <span className={styles.hint}>
            Önce bir monitor eklemelisiniz: pencere en az bir servisi kapsamak
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
        <span className={styles.label}>Açıklama (opsiyonel)</span>
        <textarea
          className={`${styles.input} ${styles.textarea}`}
          value={description}
          onChange={(event) => setDescription(event.target.value)}
          placeholder="Yükseltme sırasında kısa kesintiler olabilir."
          aria-invalid={fieldErrors.description ? true : undefined}
        />
        {fieldErrors.description && (
          <span className={styles.fieldError}>{fieldErrors.description}</span>
        )}
      </label>

      <div className={styles.actions}>
        <button className={styles.submit} type="submit" disabled={submitting}>
          {submitting ? 'Kaydediliyor…' : 'Pencereyi tanımla'}
        </button>
      </div>
    </form>
  );
}
