import type { PR } from '@/types/pr';
import type { ServerStatus } from '@/types/status';
import type { ServerWebSocketMessage } from './websocket';

export function applyPRWebSocketMessage(oldData: PR[] | undefined, message: ServerWebSocketMessage): PR[] | undefined {
  if (!oldData) return oldData;
  if (!message.payload) return oldData;

  switch (message.type) {
    case 'pr_created': {
      const newPR = message.payload as PR;
      if (!newPR) return oldData;

      if (oldData.some(pr => pr.number === newPR.number && pr.repo === newPR.repo && pr.owner === newPR.owner)) {
        return oldData.map(pr =>
          pr.number === newPR.number && pr.repo === newPR.repo && pr.owner === newPR.owner ? newPR : pr
        );
      }

      return [newPR, ...oldData];
    }

    case 'pr_updated': {
      const updatedPR = message.payload as PR;
      return oldData.map((pr) =>
        pr.number === updatedPR.number && pr.repo === updatedPR.repo && pr.owner === updatedPR.owner
          ? { ...updatedPR, is_mine: pr.is_mine }
          : pr
      );
    }

    case 'pr_deleted': {
      const { owner, repo, number } = message.payload as Pick<PR, 'owner' | 'repo' | 'number'>;
      return oldData.filter((pr) =>
        !(pr.number === number && pr.repo === repo && pr.owner === owner)
      );
    }

    default:
      return oldData;
  }
}

export function applyStatusWebSocketMessage(oldData: ServerStatus | undefined, message: ServerWebSocketMessage): ServerStatus | undefined {
  if (message.type !== 'status_snapshot' || !message.payload) {
    return oldData;
  }

  return {
    ...(message.payload as ServerStatus),
    snapshot_received_at_ms: Date.now(),
  };
}
