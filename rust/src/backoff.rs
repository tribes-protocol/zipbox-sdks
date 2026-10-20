//! Exponential backoff with full jitter.

use std::time::Duration;

use rand::Rng;

/// An exponential backoff ladder: `base * 2^attempt`, capped, with jitter.
///
/// Used for request retries (bounded by `max_retries`) and for PTY reconnects,
/// which retry forever and reset the ladder only when a frame is received.
#[derive(Debug, Clone)]
pub(crate) struct Backoff {
    base: Duration,
    cap: Duration,
    attempt: u32,
}

impl Backoff {
    pub fn new(base: Duration, cap: Duration) -> Backoff {
        Backoff {
            base,
            cap,
            attempt: 0,
        }
    }

    /// The ceiling of the next delay, before jitter.
    pub fn ceiling(&self) -> Duration {
        let factor = 1u32.checked_shl(self.attempt.min(30)).unwrap_or(u32::MAX);
        self.base.saturating_mul(factor).min(self.cap)
    }

    /// The next delay (uniform in `[ceiling/2, ceiling]`), advancing the ladder.
    pub fn next_delay(&mut self) -> Duration {
        let ceiling = self.ceiling();
        self.attempt = self.attempt.saturating_add(1);
        if ceiling.is_zero() {
            return ceiling;
        }
        let half = ceiling / 2;
        let jitter = rand::thread_rng().gen_range(0..=half.as_millis().max(1) as u64);
        half + Duration::from_millis(jitter)
    }

    /// Back to the first rung.
    pub fn reset(&mut self) {
        self.attempt = 0;
    }

    pub fn attempt(&self) -> u32 {
        self.attempt
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn grows_caps_and_resets() {
        let mut b = Backoff::new(Duration::from_millis(500), Duration::from_secs(30));
        let mut last = Duration::ZERO;
        for _ in 0..20 {
            let ceiling = b.ceiling();
            let d = b.next_delay();
            assert!(d <= ceiling && d >= ceiling / 2);
            assert!(ceiling >= last);
            last = ceiling;
        }
        assert_eq!(b.ceiling(), Duration::from_secs(30));
        assert_eq!(b.attempt(), 20);
        b.reset();
        assert_eq!(b.ceiling(), Duration::from_millis(500));
    }

    #[test]
    fn zero_base_never_sleeps() {
        let mut b = Backoff::new(Duration::ZERO, Duration::ZERO);
        assert_eq!(b.next_delay(), Duration::ZERO);
    }
}
