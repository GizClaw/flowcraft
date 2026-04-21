export interface Dataset {
  id: string;
  name: string;
  description?: string;
  agent_id?: string;
  document_count?: number;
  l0_abstract?: string;
  created_at: string;
  updated_at: string;
}

export interface DatasetDocument {
  id: string;
  dataset_id: string;
  name: string;
  content: string;
  chunk_count?: number;
  l0_abstract?: string;
  l1_overview?: string;
  processing_status?: 'pending' | 'processing' | 'completed' | 'failed';
  created_at: string;
}

export interface CreateDatasetRequest {
  name: string;
  description?: string;
  agent_id?: string;
}

export interface AddDocumentRequest {
  name: string;
  content: string;
}

export type KnowledgeLayer = 'L0' | 'L1' | 'L2';

export interface QueryDatasetRequest {
  query: string;
  top_k?: number;
  max_layer?: KnowledgeLayer;
}

export interface QueryResult {
  document_id: string;
  document_name: string;
  content: string;
  score: number;
  layer?: KnowledgeLayer;
  chunk_index?: number;
}
