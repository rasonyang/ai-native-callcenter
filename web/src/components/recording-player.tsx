import { Loader2, Pause, Play, RotateCcw, RotateCw } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { formatDuration } from '@/lib/ledger'

const BAR_COUNT = 72
const SKIP_SEC = 10

/**
 * One recording, heard in place: a waveform to scrub, transport buttons and
 * the clock. The audio is fetched once — the same bytes draw the waveform and
 * feed playback — so scrubbing never goes back to the server.
 */
export function RecordingPlayer({ src, durationSec = 0 }: { src: string; durationSec?: number }) {
  const { t } = useTranslation()
  const audioRef = useRef<HTMLAudioElement | null>(null)
  const [objectUrl, setObjectUrl] = useState<string | null>(null)
  const [peaks, setPeaks] = useState<number[] | null>(null)
  const [isFailed, setFailed] = useState(false)
  const [isPlaying, setPlaying] = useState(false)
  const [position, setPosition] = useState(0)
  const [duration, setDuration] = useState(durationSec)

  useEffect(() => {
    let cancelled = false
    let url: string | null = null
    fetch(src)
      .then((response) => {
        if (!response.ok) throw new Error(String(response.status))
        return response.blob()
      })
      .then(async (blob) => {
        if (cancelled) return
        url = URL.createObjectURL(blob)
        setObjectUrl(url)
        // The waveform is a nicety: an environment without decodeAudioData,
        // or a file it cannot parse, still plays over flat bars.
        try {
          const context = new AudioContext()
          const decoded = await context.decodeAudioData(await blob.arrayBuffer())
          void context.close()
          if (cancelled) return
          setPeaks(peaksOf(decoded))
          setDuration(decoded.duration)
        } catch {
          /* flat bars */
        }
      })
      .catch(() => {
        if (!cancelled) setFailed(true)
      })
    return () => {
      cancelled = true
      if (url) URL.revokeObjectURL(url)
    }
  }, [src])

  const seekTo = (sec: number) => {
    const audio = audioRef.current
    if (!audio || !duration) return
    const clamped = Math.min(Math.max(sec, 0), duration)
    audio.currentTime = clamped
    setPosition(clamped)
  }

  const toggle = () => {
    const audio = audioRef.current
    if (!audio) return
    if (isPlaying) {
      audio.pause()
    } else {
      audio.play()?.catch(() => setFailed(true))
    }
  }

  if (isFailed) {
    return <p className="text-xs text-muted-foreground">{t('player.failed')}</p>
  }
  if (!objectUrl) {
    return (
      <p className="flex h-12 items-center text-xs text-muted-foreground">
        <Loader2 className="mr-1.5 size-4 animate-spin" />
        {t('common.loading')}
      </p>
    )
  }

  const bars = peaks ?? Array.from({ length: BAR_COUNT }, () => 0.35)
  const progress = duration > 0 ? position / duration : 0

  return (
    <div>
      <audio
        ref={audioRef}
        src={objectUrl}
        preload="auto"
        className="hidden"
        onPlay={() => setPlaying(true)}
        onPause={() => setPlaying(false)}
        onEnded={() => setPlaying(false)}
        onTimeUpdate={(event) => setPosition(event.currentTarget.currentTime)}
        onLoadedMetadata={(event) => {
          // The decoded buffer already set the truth; metadata fills in when
          // decoding was unavailable.
          if (Number.isFinite(event.currentTarget.duration) && !peaks) {
            setDuration(event.currentTarget.duration)
          }
        }}
      />

      <div
        role="slider"
        tabIndex={0}
        aria-label={t('player.seek')}
        aria-valuemin={0}
        aria-valuemax={Math.round(duration)}
        aria-valuenow={Math.round(position)}
        aria-valuetext={formatDuration(position)}
        className="flex h-12 cursor-pointer items-center gap-px rounded-md outline-none focus-visible:ring-2 focus-visible:ring-ring"
        onClick={(event) => {
          const rect = event.currentTarget.getBoundingClientRect()
          seekTo(((event.clientX - rect.left) / rect.width) * duration)
        }}
        onKeyDown={(event) => {
          if (event.key === 'ArrowLeft') seekTo(position - 5)
          if (event.key === 'ArrowRight') seekTo(position + 5)
          if (event.key === ' ' || event.key === 'Enter') {
            event.preventDefault()
            toggle()
          }
        }}
      >
        {bars.map((peak, index) => (
          <span
            key={index}
            className={`min-h-0.5 flex-1 rounded-[1px] ${
              index / bars.length < progress ? 'bg-primary' : 'bg-muted-foreground/25'
            }`}
            style={{ height: `${Math.max(peak * 100, 4)}%` }}
          />
        ))}
      </div>

      <div className="mt-1 flex items-center gap-1">
        <Button
          size="sm"
          variant="ghost"
          className="size-8 p-0"
          title={t('player.back10')}
          aria-label={t('player.back10')}
          onClick={() => seekTo(position - SKIP_SEC)}
        >
          <RotateCcw />
        </Button>
        <Button
          size="sm"
          variant="outline"
          className="size-8 p-0"
          title={isPlaying ? t('player.pause') : t('player.play')}
          aria-label={isPlaying ? t('player.pause') : t('player.play')}
          onClick={toggle}
        >
          {isPlaying ? <Pause /> : <Play />}
        </Button>
        <Button
          size="sm"
          variant="ghost"
          className="size-8 p-0"
          title={t('player.forward10')}
          aria-label={t('player.forward10')}
          onClick={() => seekTo(position + SKIP_SEC)}
        >
          <RotateCw />
        </Button>
        <span className="ml-auto text-xs tabular text-muted-foreground">
          {formatDuration(position)} / {formatDuration(duration)}
        </span>
      </div>
    </div>
  )
}

/**
 * Collapses the decoded audio into one peak per bar, normalised to the
 * loudest. Every channel counts: call recordings are stereo with one party a
 * side, and a waveform read off one channel would go flat whenever the other
 * party speaks.
 */
function peaksOf(buffer: AudioBuffer): number[] {
  const channels = Array.from({ length: buffer.numberOfChannels }, (_, c) => buffer.getChannelData(c))
  const bucket = Math.max(1, Math.floor(buffer.length / BAR_COUNT))
  const peaks: number[] = []
  for (let bar = 0; bar < BAR_COUNT; bar++) {
    let max = 0
    const start = bar * bucket
    const end = Math.min(start + bucket, buffer.length)
    for (const samples of channels) {
      for (let i = start; i < end; i++) {
        const value = Math.abs(samples[i])
        if (value > max) max = value
      }
    }
    peaks.push(max)
  }
  const loudest = Math.max(...peaks, 0.01)
  return peaks.map((peak) => peak / loudest)
}
