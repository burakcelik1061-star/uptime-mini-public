import type { ReactElement } from 'react';

import type { IncidentImpact, IncidentStatus } from '@/lib/api-types';
import styles from './IncidentBadges.module.css';

/**
 * Incident'ın durumu ve etkisi.
 *
 * KURAL (0.2, kullanıcı şartı): durum ASLA sadece renkle anlatılmaz —
 * `StatusBadge` ile aynı disiplin. Her değerin kendi ŞEKLİ ve METİN etiketi
 * var; etki rozetinde ayrıca DOLU KARE SAYISI (1–3) ikinci bir sinyal.
 */

const STATUS_VISUALS: Record<
  IncidentStatus,
  { label: string; className: string; shape: ReactElement }
> = {
  investigating: {
    label: 'Araştırılıyor',
    className: styles.investigating,
    shape: (
      <path
        d="M7 2.5a4.5 4.5 0 1 0 2.9 7.95l3.1 3.1 1.1-1.1-3.1-3.1A4.5 4.5 0 0 0 7 2.5z"
        fill="currentColor"
      />
    ),
  },
  identified: {
    label: 'Sebebi bulundu',
    className: styles.identified,
    shape: (
      <path
        d="M8 1.5 15 14H1z M8 6v4 M8 11.5v.8"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    ),
  },
  monitoring: {
    label: 'İzleniyor',
    className: styles.monitoring,
    shape: (
      <path
        d="M1.5 8s2.4-4 6.5-4 6.5 4 6.5 4-2.4 4-6.5 4-6.5-4-6.5-4z M8 6.2a1.8 1.8 0 1 0 0 3.6 1.8 1.8 0 0 0 0-3.6z"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinejoin="round"
      />
    ),
  },
  resolved: {
    label: 'Çözüldü',
    className: styles.resolved,
    shape: (
      <path
        d="M3 8.5l3.2 3.2L13 5"
        fill="none"
        stroke="currentColor"
        strokeWidth="2.2"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    ),
  },
};

export function IncidentStatusBadge({ status }: { status: IncidentStatus }) {
  const visual = STATUS_VISUALS[status];

  return (
    <span className={`${styles.badge} ${visual.className}`}>
      <svg
        width="16"
        height="16"
        viewBox="0 0 16 16"
        aria-hidden="true"
        focusable="false"
      >
        {visual.shape}
      </svg>
      {visual.label}
    </span>
  );
}

/** Etki seviyesi: metin + dolu kare sayısı (renk tek sinyal değil). */
const IMPACT_VISUALS: Record<
  IncidentImpact,
  { label: string; className: string; filled: number }
> = {
  none: { label: 'Etkisiz', className: styles.impactNone, filled: 0 },
  minor: { label: 'Küçük', className: styles.impactMinor, filled: 1 },
  major: { label: 'Büyük', className: styles.impactMajor, filled: 2 },
  critical: { label: 'Kritik', className: styles.impactCritical, filled: 3 },
};

const IMPACT_LEVELS = [0, 1, 2] as const;

export function ImpactBadge({ impact }: { impact: IncidentImpact }) {
  const visual = IMPACT_VISUALS[impact];

  return (
    <span className={`${styles.badge} ${visual.className}`}>
      <svg
        width="18"
        height="10"
        viewBox="0 0 18 10"
        aria-hidden="true"
        focusable="false"
      >
        {IMPACT_LEVELS.map((level) => (
          <rect
            key={level}
            x={level * 6.5}
            y={0.5}
            width="5"
            height="9"
            rx="1"
            fill={level < visual.filled ? 'currentColor' : 'none'}
            stroke="currentColor"
            strokeWidth="1"
          />
        ))}
      </svg>
      {visual.label}
    </span>
  );
}
