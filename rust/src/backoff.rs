//! Exponential backoff with full jitter.

use std::time::Duration;

use rand::Rng;

/// How much each re-read of a readiness wait grows. A box turns ready about a
/// second after it serves, and nothing tells the client when, so each rung is time
/// spent asleep after it did. Doubling overshot by up to several seconds; 1.25 keeps
/// the same base, cap and jitter and reaches the cap gently.
const POLL_FACTOR: f64 = 1.25;

/// An exponential backoff ladder: `base * factor^attempt`, capped, with jitter.
///
/// Used for request retries (bounded by `max_retries`) and for PTY reconnects,
/// which retry forever and reset the ladder only when a frame is received, both
/// doubling; and for readiness waits, growing by [`POLL_FACTOR`].
#[derive(Debug, Clone)]
pub(crate) struct Backoff {
    base: Duration,
    cap: Duration,
    factor: f64,
    attempt: u32,
}

impl Backoff {
    pub fn new(base: Duration, cap: Duration) -> Backoff {
        Backoff {
            base,
            cap,
            factor: 2.0,
            attempt: 0,
        }
    }

    /// The ladder a readiness wait re-reads a box on.
    pub fn poll(base: Duration, cap: Duration) -> Backoff {
        Backoff {
            factor: POLL_FACTOR,
            ..Backoff::new(base, cap)
        }
    }

    /// The ceiling of the next delay, before jitter.
    pub fn ceiling(&self) -> Duration {
        let scaled = self.base.as_secs_f64() * self.factor.powi(self.attempt.min(64) as i32);
        if scaled >= self.cap.as_secs_f64() {
            self.cap
        } else {
            Duration::from_secs_f64(scaled)
        }
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

    // A box turns ready ~1 s after it serves; doubling slept up to seconds past it.
    #[test]
    fn poll_grows_by_a_quarter_to_the_cap() {
        let mut b = Backoff::poll(Duration::from_secs(1), Duration::from_secs(10));
        let want = [
            1.0,
            1.25,
            1.5625,
            1.953125,
            2.44140625,
            3.0517578125,
            3.814697,
            4.768372,
            5.960464,
            7.450581,
            9.313226,
            10.0,
            10.0,
            10.0,
        ];
        for w in want {
            let ceiling = b.ceiling();
            assert!(
                (ceiling.as_secs_f64() - w).abs() < 1e-6,
                "{ceiling:?} != {w}s"
            );
            let d = b.next_delay();
            assert!(d <= ceiling && d >= ceiling / 2);
        }
        for _ in 0..1000 {
            b.next_delay();
        }
        assert_eq!(b.ceiling(), Duration::from_secs(10));
    }

    #[test]
    fn zero_base_never_sleeps() {
        let mut b = Backoff::new(Duration::ZERO, Duration::ZERO);
        assert_eq!(b.next_delay(), Duration::ZERO);
    }
}
