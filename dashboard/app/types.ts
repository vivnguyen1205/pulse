export type AlertEpisode = {
  id: number;
  host: string;
  name: string;
  detector: string;
  started_at: number;
  ended_at: number | null;
  peak_value: number;
  peak_score: number;
};
